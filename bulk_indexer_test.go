// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package docappender_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"

	"github.com/elastic/go-docappender/v2"
	"github.com/elastic/go-docappender/v2/docappendertest"
	"github.com/stretchr/testify/require"
)

func TestBulkIndexer(t *testing.T) {
	for _, tc := range []struct {
		Name             string
		CompressionLevel int
	}{
		{Name: "no_compression", CompressionLevel: gzip.NoCompression},
		{Name: "default_compression", CompressionLevel: gzip.DefaultCompression},
		{Name: "most_compression", CompressionLevel: gzip.BestCompression},
		{Name: "speed_compression", CompressionLevel: gzip.BestSpeed},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			var esFailing atomic.Bool
			client := docappendertest.NewMockElasticsearchClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, result := docappendertest.DecodeBulkRequest(r)
				if esFailing.Load() {
					for _, itemsMap := range result.Items {
						for k, item := range itemsMap {
							result.HasErrors = true
							item.Status = http.StatusTooManyRequests
							item.Error.Type = "simulated_es_error"
							item.Error.Reason = "for testing"
							itemsMap[k] = item
						}
					}
				}
				json.NewEncoder(w).Encode(result)
			})
			indexer, err := docappender.NewBulkIndexer(docappender.BulkIndexerConfig{
				Client:                client,
				MaxDocumentRetries:    100_000, // infinite for testing purpose
				RetryOnDocumentStatus: []int{http.StatusTooManyRequests},
				CompressionLevel:      tc.CompressionLevel,
			})
			require.NoError(t, err)

			generateLoad := func(count int) {
				for i := 0; i < count; i++ {
					require.NoError(t, indexer.Add(docappender.BulkIndexerItem{
						Index: "testidx",
						Body: newJSONReader(map[string]any{
							"@timestamp": time.Now().Format(docappendertest.TimestampFormat),
						}),
					}))
				}
			}

			itemCount := 1_000
			generateLoad(itemCount)

			// All items should be successfully flushed
			uncompressed := indexer.UncompressedLen()
			uncompressedDocSize := uncompressed / itemCount
			stat, err := indexer.Flush(context.Background())
			require.NoError(t, err)
			require.Equal(t, int64(itemCount), stat.Indexed)
			require.Equal(t, uncompressed, indexer.BytesUncompressedFlushed())

			// nothing is in the buffer if all succeeded
			require.Equal(t, 0, indexer.Len())
			require.Equal(t, 0, indexer.UncompressedLen())

			// Simulate ES failure, all items should be enqueued for retries
			esFailing.Store(true)
			generateLoad(itemCount)
			require.Equal(t, itemCount, indexer.Items())

			for i := 0; i < 10; i++ {
				stat, err := indexer.Flush(context.Background())
				require.NoError(t, err)
				require.Equal(t, int64(0), stat.Indexed)
				require.Equal(t, itemCount, len(stat.FailedDocs))
				require.Equal(t, int64(itemCount), stat.RetriedDocs)

				// all the flushed bytes are now in the buffer again to be retried
				require.Equal(t, indexer.UncompressedLen(), indexer.BytesUncompressedFlushed())
				// Generate more load, all these items should be enqueued for retries
				generateLoad(10)
				itemCount += 10
				require.Equal(t, itemCount, indexer.Items())
				expectedBufferedSize := indexer.BytesUncompressedFlushed() + (10 * uncompressedDocSize)
				require.Equal(t, expectedBufferedSize, indexer.UncompressedLen())
			}

			uncompressedSize := indexer.UncompressedLen()
			// Recover ES and ensure all items are indexed
			esFailing.Store(false)
			stat, err = indexer.Flush(context.Background())
			require.NoError(t, err)
			require.Equal(t, int64(itemCount), stat.Indexed)
			require.Equal(t, uncompressedSize, indexer.BytesUncompressedFlushed())
			// no documents to retry so buffer should be empty
			require.Equal(t, 0, indexer.Len())
			require.Equal(t, 0, indexer.UncompressedLen())
		})
	}
}

func TestRedactErrorReason(t *testing.T) {
	errMsg := "error_reason_invalid. Preview of field's value: 'failed to parse value'"
	tests := []struct {
		name       string
		fullReason bool
		message    string
	}{
		{
			name:    "redact by default",
			message: "error_reason_invalid",
		},
		{
			name:       "return full error reason",
			fullReason: true,
			message:    "error_reason_invalid. Preview of field's value: 'failed to parse value'",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := docappendertest.NewMockElasticsearchClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, result := docappendertest.DecodeBulkRequest(r)
				for _, itemsMap := range result.Items {
					for k, item := range itemsMap {
						result.HasErrors = true
						item.Status = http.StatusBadRequest
						item.Index = "an_index"
						item.Error.Type = "error_type"
						item.Error.Reason = errMsg
						itemsMap[k] = item
					}
				}
				json.NewEncoder(w).Encode(result)
			})
			indexer, err := docappender.NewBulkIndexer(docappender.BulkIndexerConfig{
				Client:                 client,
				CaptureFullErrorReason: tc.fullReason,
			})
			require.NoError(t, err)

			require.NoError(t, indexer.Add(docappender.BulkIndexerItem{
				Index: "testidx",
				Body: newJSONReader(map[string]any{
					"@timestamp": time.Now().Format(docappendertest.TimestampFormat),
				}),
			}))

			stat, err := indexer.Flush(context.Background())
			require.NoError(t, err)
			require.Equal(t, 1, len(stat.FailedDocs))
			require.Equal(t, tc.message, stat.FailedDocs[0].Error.Reason)
		})
	}
}

func BenchmarkRedaction(b *testing.B) {
	errMsg := "error_reason_invalid. Preview of field's value: 'failed to parse value'"
	client := docappendertest.NewMockElasticsearchClient(b, func(w http.ResponseWriter, r *http.Request) {
		_, result := docappendertest.DecodeBulkRequest(r)
		for _, itemsMap := range result.Items {
			for k, item := range itemsMap {
				result.HasErrors = true
				item.Status = http.StatusBadRequest
				item.Index = "an_index"
				item.Error.Type = "error_type"
				item.Error.Reason = errMsg
				itemsMap[k] = item
			}
		}
		json.NewEncoder(w).Encode(result)
	})
	indexer, err := docappender.NewBulkIndexer(docappender.BulkIndexerConfig{
		Client:                 client,
		CaptureFullErrorReason: false,
	})
	require.NoError(b, err)
	generateLoad := func(count int) {
		for i := 0; i < count; i++ {
			require.NoError(b, indexer.Add(docappender.BulkIndexerItem{
				Index: "testidx",
				Body: newJSONReader(map[string]any{
					"@timestamp": time.Now().Format(docappendertest.TimestampFormat),
				}),
			}))
		}
	}
	for i := 0; i < b.N; i++ {
		itemCount := 1_000
		generateLoad(itemCount)
		_, err := indexer.Flush(context.Background())
		if err != nil {
			b.FailNow()
		}
	}
}
