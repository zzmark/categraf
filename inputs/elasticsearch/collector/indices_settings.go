// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"encoding/json"
	"flashcat.cloud/categraf/pkg/filter"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// IndicesSettings information struct
type IndicesSettings struct {
	client               *http.Client
	url                  *url.URL
	indicesIncluded      []string
	numMostRecentIndices int
	indexMatchers        map[string]filter.Filter

	up              prometheus.Gauge
	readOnlyIndices prometheus.Gauge

	totalScrapes, jsonParseFailures prometheus.Counter
	metrics                         []*indicesSettingsMetric
}

var (
	defaultIndicesTotalFieldsLabels = []string{"index"}
	defaultTotalFieldsValue         = 1000 //es default configuration for total fields
	defaultDateCreation             = 0    //es index default creation date
)

type indicesSettingsMetric struct {
	Type  prometheus.ValueType
	Desc  *prometheus.Desc
	Value func(indexSettings Settings) float64
}

// NewIndicesSettings defines Indices Settings Prometheus metrics
func NewIndicesSettings(client *http.Client, url *url.URL, indicesIncluded []string, numMostRecentIndices int, indexMatchers map[string]filter.Filter) *IndicesSettings {
	return &IndicesSettings{
		client:               client,
		url:                  url,
		indicesIncluded:      indicesIncluded,
		numMostRecentIndices: numMostRecentIndices,
		indexMatchers:        indexMatchers,

		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prometheus.BuildFQName(namespace, "indices_settings_stats", "up"),
			Help: "Was the last scrape of the Elasticsearch Indices Settings endpoint successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: prometheus.BuildFQName(namespace, "indices_settings_stats", "total_scrapes"),
			Help: "Current total Elasticsearch Indices Settings scrapes.",
		}),
		readOnlyIndices: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prometheus.BuildFQName(namespace, "indices_settings_stats", "read_only_indices"),
			Help: "Current number of read only indices within cluster",
		}),
		jsonParseFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: prometheus.BuildFQName(namespace, "indices_settings_stats", "json_parse_failures"),
			Help: "Number of errors while parsing JSON.",
		}),
		metrics: []*indicesSettingsMetric{
			{
				Type: prometheus.GaugeValue,
				Desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, "indices_settings", "total_fields"),
					"index mapping setting for total_fields",
					defaultIndicesTotalFieldsLabels, nil,
				),
				Value: func(indexSettings Settings) float64 {
					val, err := strconv.ParseFloat(indexSettings.IndexInfo.Mapping.TotalFields.Limit, 64)
					if err != nil {
						return float64(defaultTotalFieldsValue)
					}
					return val
				},
			},
			{
				Type: prometheus.GaugeValue,
				Desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, "indices_settings", "replicas"),
					"index setting number_of_replicas",
					defaultIndicesTotalFieldsLabels, nil,
				),
				Value: func(indexSettings Settings) float64 {
					val, err := strconv.ParseFloat(indexSettings.IndexInfo.NumberOfReplicas, 64)
					if err != nil {
						return float64(defaultTotalFieldsValue)
					}
					return val
				},
			},
			{
				Type: prometheus.GaugeValue,
				Desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, "indices_settings", "creation_timestamp_seconds"),
					"index setting creation_date",
					defaultIndicesTotalFieldsLabels, nil,
				),
				Value: func(indexSettings Settings) float64 {
					val, err := strconv.ParseFloat(indexSettings.IndexInfo.CreationDate, 64)
					if err != nil {
						return float64(defaultDateCreation)
					}
					return val / 1000.0
				},
			},
		},
	}
}

// Describe add Snapshots metrics descriptions
func (cs *IndicesSettings) Describe(ch chan<- *prometheus.Desc) {
	ch <- cs.up.Desc()
	ch <- cs.totalScrapes.Desc()
	ch <- cs.readOnlyIndices.Desc()
	ch <- cs.jsonParseFailures.Desc()
}

func (cs *IndicesSettings) getAndParseURL(u *url.URL, data interface{}) error {
	res, err := cs.client.Get(u.String())
	if err != nil {
		return fmt.Errorf("failed to get from %s://%s:%s%s: %s",
			u.Scheme, u.Hostname(), u.Port(), u.Path, err)
	}

	defer func() {
		err = res.Body.Close()
		if err != nil {
			log.Println("failed to close http.Client, err :", err)
		}
	}()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP Request failed with code %d", res.StatusCode)
	}

	bts, err := io.ReadAll(res.Body)
	if err != nil {
		cs.jsonParseFailures.Inc()
		return err
	}

	if err := json.Unmarshal(bts, data); err != nil {
		cs.jsonParseFailures.Inc()
		return err
	}
	return nil
}

func (cs *IndicesSettings) fetchAndDecodeIndicesSettings() (IndicesSettingsResponse, error) {

	u := *cs.url
	//add indices filter
	if len(cs.indicesIncluded) == 0 {
		u.Path = path.Join(u.Path, "/_all/_settings")
	} else {
		u.Path = path.Join(u.Path, "/"+strings.Join(cs.indicesIncluded, ",")+"/_settings")
	}
	var asr IndicesSettingsResponse
	err := cs.getAndParseURL(&u, &asr)
	if err != nil {
		return asr, err
	}

	return asr, err
}

// Collect gets all indices settings metric values
func (cs *IndicesSettings) Collect(ch chan<- prometheus.Metric) {

	cs.totalScrapes.Inc()
	defer func() {
		ch <- cs.up
		ch <- cs.totalScrapes
		ch <- cs.jsonParseFailures
		ch <- cs.readOnlyIndices
	}()

	asr, err := cs.fetchAndDecodeIndicesSettings()
	if err != nil {
		cs.readOnlyIndices.Set(0)
		cs.up.Set(0)
		log.Println("failed to fetch and decode cluster settings stats, err :", err)
		return
	}

	//add config i.numMostRecentIndices process code
	asr = cs.gatherIndividualIndicesStats(asr)

	cs.up.Set(1)

	var c int
	for indexName, value := range asr {
		if value.Settings.IndexInfo.Blocks.ReadOnly == "true" {
			c++
		}
		for _, metric := range cs.metrics {
			ch <- prometheus.MustNewConstMetric(
				metric.Desc,
				metric.Type,
				metric.Value(value.Settings),
				indexName,
			)
		}
	}
	cs.readOnlyIndices.Set(float64(c))
}

// gatherSortedIndicesStats gathers stats for all indices in no particular order.
func (cs *IndicesSettings) gatherIndividualIndicesStats(asr IndicesSettingsResponse) IndicesSettingsResponse {
	newIndicesSettings := make(map[string]Index)

	// Sort indices into buckets based on their configured prefix, if any matches.
	categorizedIndexNames := cs.categorizeIndices(asr)
	for _, matchingIndices := range categorizedIndexNames {
		// Establish the number of each category of indices to use. User can configure to use only the latest 'X' amount.
		indicesCount := len(matchingIndices)
		indicesToTrackCount := indicesCount

		// Sort the indices if configured to do so.
		if cs.numMostRecentIndices > 0 {
			if cs.numMostRecentIndices < indicesToTrackCount {
				indicesToTrackCount = cs.numMostRecentIndices
			}
			sort.Strings(matchingIndices)
		}

		// Gather only the number of indexes that have been configured, in descending order (most recent, if date-stamped).
		for i := indicesCount - 1; i >= indicesCount-indicesToTrackCount; i-- {
			indexName := matchingIndices[i]
			newIndicesSettings[indexName] = asr[indexName]
		}
	}
	//return new IndicesSettingsResponse
	var isr IndicesSettingsResponse
	isr = newIndicesSettings
	return isr
}

func (cs *IndicesSettings) categorizeIndices(asr IndicesSettingsResponse) map[string][]string {
	categorizedIndexNames := make(map[string][]string, len(asr))

	// If all indices are configured to be gathered, bucket them all together.
	if len(cs.indicesIncluded) == 0 || cs.indicesIncluded[0] == "_all" {
		for indexName := range asr {
			categorizedIndexNames["_all"] = append(categorizedIndexNames["_all"], indexName)
		}

		return categorizedIndexNames
	}

	// Bucket each returned index with its associated configured index (if any match).
	for indexName := range asr {
		match := indexName
		for name, matcher := range cs.indexMatchers {
			// If a configured index matches one of the returned indexes, mark it as a match.
			if matcher.Match(match) {
				match = name
				break
			}
		}

		// Bucket all matching indices together for sorting.
		categorizedIndexNames[match] = append(categorizedIndexNames[match], indexName)
	}

	return categorizedIndexNames
}
