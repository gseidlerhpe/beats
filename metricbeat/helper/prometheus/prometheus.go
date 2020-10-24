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

package prometheus

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/metricbeat/helper"
	"github.com/elastic/beats/metricbeat/mb"
)

// Prometheus helper retrieves prometheus formatted metrics
type Prometheus interface {
	// GetFamilies requests metric families from prometheus endpoint and returns them
	GetFamilies(familyPrefix []string) ([]*dto.MetricFamily, error)

	GetProcessedMetrics(mapping *MetricsMapping) ([]common.MapStr, error)

	ReportProcessedMetrics(mapping *MetricsMapping, r mb.ReporterV2)
}

type prometheus struct {
	httpfetcher
}

type httpfetcher interface {
	FetchResponse() (*http.Response, error)
}

const (
	// maximum size of metrics processed in metrics processing go routine
	maxMetricsSliceSize = 5000

	// maximum number of GetFamilies funs
	maxGetFamiliesSubFuns = 16
)

// NewPrometheusClient creates new prometheus helper
func NewPrometheusClient(base mb.BaseMetricSet) (Prometheus, error) {
	http, err := helper.NewHTTP(base)
	if err != nil {
		return nil, err
	}
	return &prometheus{http}, nil
}

// GetFamilies requests metric families from prometheus endpoint and returns them
func (p *prometheus) GetFamilies(familyPrefix []string) ([]*dto.MetricFamily, error) {
	resp, err := p.FetchResponse()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	format := expfmt.ResponseFormat(resp.Header)
	if format == "" {
		return nil, fmt.Errorf("Invalid format for response of response")
	}

	decoder := expfmt.NewDecoder(resp.Body, format)
	if decoder == nil {
		return nil, fmt.Errorf("Unable to create decoder to decode response")
	}

	families := []*dto.MetricFamily{}
	var wg sync.WaitGroup
	var decoderMutex = &sync.Mutex{}
	var familiesMutex = &sync.Mutex{}
	var done = false
	var count = 0
	for !done && count < maxGetFamiliesSubFuns {
		wg.Add(1)
		count++
		go func(id int) {
			defer wg.Done()
			for !done {
				mf := &dto.MetricFamily{}
				decoderMutex.Lock()
				err = decoder.Decode(mf)
				decoderMutex.Unlock()
				if err != nil {
					if err == io.EOF {
						done = true
					}
					return
				}

				if familyPrefix == nil {
					familiesMutex.Lock()
					families = append(families, mf)
					familiesMutex.Unlock()
				} else {
					found := false
					for _, prefix := range familyPrefix {
						if strings.HasPrefix(mf.GetName(), prefix) {
							familiesMutex.Lock()
							families = append(families, mf)
							familiesMutex.Unlock()
							found = true
							break
						}
					}
					if !found {
						mf.Reset()
					}
				}
			}
		}(count)
	}
	wg.Wait()

	return families, nil
}

// KeyLabelKeys defines the primary and secondary key labels
type KeyLabelKeys struct {
	// Primary key label keys
	Primary map[string]struct{}

	// Secondary key label keys
	Secondary map[string]struct{}
}

// MetricsMapping defines mapping settings for Prometheus metrics, to be used with `GetProcessedMetrics`
type MetricsMapping struct {
	// Metrics Family name prefix
	FamilyPrefix []string

	// Metrics translates from from prometheus metric name to Metricbeat fields
	Metrics map[string]MetricMap

	// InfoMetrics translates from prometheus info metric name to Metricbeat fields
	InfoMetrics map[string]MetricMap

	// Labels translate from prometheus label names to Metricbeat fields
	Labels map[string]LabelMap

	// Primary and secondary key label keys (optional)
	KeyLabels KeyLabelKeys

	// ExtraFields adds the given fields to all events coming from `GetProcessedMetrics`
	ExtraFields map[string]string
}

// processFamilyMetrics
func processFamilyMetrics(eventsMap *eventsMaps, mapping *MetricsMapping, families []*dto.MetricFamily, infoMetrics *infoMetrics) {
	for _, family := range families {
		var wg sync.WaitGroup
		var metricMapping map[string]MetricMap

		if infoMetrics != nil {
			metricMapping = mapping.InfoMetrics
		} else {
			metricMapping = mapping.Metrics
		}

		// Ignore unknown metrics
		metricMap, ok := metricMapping[family.GetName()]
		if !ok {
			continue
		}

		familyMetrics := family.GetMetric()
		sliceSize := len(familyMetrics)
		if sliceSize > maxMetricsSliceSize {
			sliceSize = maxMetricsSliceSize
		}

		for i := 0; i < len(familyMetrics); i += sliceSize {
			var slice []*dto.Metric
			if i+sliceSize < len(familyMetrics) {
				slice = familyMetrics[i : i+sliceSize]
			} else {
				slice = familyMetrics[i:]
			}
			wg.Add(1)

			go func(idMetric int, metricMap MetricMap, metrics []*dto.Metric) {
				for _, metric := range metrics {
					field := metricMap.GetField()
					value := metricMap.GetValue(metric)

					// Ignore retrieval errors (bad conf)
					if value == nil {
						continue
					}

					// Apply extra options
					allLabels := getLabels(metric)
					for _, option := range metricMap.GetOptions() {
						field, value, allLabels = option.Process(field, value, allLabels)
					}

					primaryKeyLabels, secondaryKeyLabels, labels := getLabelsByType(mapping, allLabels)
					if infoMetrics != nil {
						labels.DeepUpdate(primaryKeyLabels)
						event := eventsMap.getEvent(primaryKeyLabels, secondaryKeyLabels)
						if event != nil {
							eventsMap.updateEvent(event, "", nil, labels)
						} else {
							// No matching event found!
							// Add metrics that have additional labels for later processing only
							if primaryKeyLabels.String() != labels.String() {
								infoMetrics.mutex.Lock()
								infoMetrics.data = append(infoMetrics.data, &infoMetricData{
									Labels: primaryKeyLabels,
									Meta:   labels,
								})
								infoMetrics.mutex.Unlock()
							}
						}
					} else {
						if field != "" {
							// Put it in the event if it's a common metric
							_, event := eventsMap.getOrCreateEvent(primaryKeyLabels, secondaryKeyLabels)
							eventsMap.updateEvent(event, field, value, labels)
						}
					}
				}
				wg.Done()
			}(i, metricMap, slice)

		}
		wg.Wait()
	}
}

// processMetrics processes non InfoMetrics families into events and build the eventsMap
func processMetrics(eventsMap *eventsMaps, mapping *MetricsMapping, families []*dto.MetricFamily) {
	processFamilyMetrics(eventsMap, mapping, families, nil)
}

// processInfoMetrics processes the InfoMetrics families and appends the data to the metrics events
// in the eventsMap
func processInfoMetrics(eventsMap *eventsMaps, mapping *MetricsMapping, families []*dto.MetricFamily) {
	infoMetrics := infoMetrics{
		data:  []*infoMetricData{},
		mutex: sync.Mutex{},
	}

	processFamilyMetrics(eventsMap, mapping, families, &infoMetrics)

	// infoMetrics contains any InfoMetrics that have been matched yet.
	// Find the matching event and append it wth the infoMetrics.
	for _, info := range infoMetrics.data {
		for _, event := range eventsMap.events {
			found := true
			for k, v := range info.Labels.Flatten() {
				value, err := event.GetValue(k)
				if err != nil || v != value {
					found = false
					break
				}
			}

			// fill info from this metric
			if found {
				event.DeepUpdate(info.Meta)
			}
		}
	}
}

func (p *prometheus) GetProcessedMetrics(mapping *MetricsMapping) ([]common.MapStr, error) {
	eventsMap := eventsMaps{
		events:     map[string]common.MapStr{},
		metricKeys: map[string]string{},
		mapMutex:   sync.Mutex{},
		eventMutex: sync.Mutex{},
	}

	families, err := p.GetFamilies(mapping.FamilyPrefix)
	if err != nil {
		return nil, err
	}

	// first process non-InfoMetrics
	processMetrics(&eventsMap, mapping, families)

	// Now process infoMetrics and fill in additional info
	processInfoMetrics(&eventsMap, mapping, families)

	// populate events array from values in eventsMap
	events := make([]common.MapStr, 0, len(eventsMap.events))
	for _, event := range eventsMap.events {
		// Add extra fields
		for k, v := range mapping.ExtraFields {
			event[k] = v
		}
		events = append(events, event)

	}

	for _, family := range families {
		family.Reset()
	}

	return events, nil
}

// infoMetricData keeps data about an infoMetric
type infoMetricData struct {
	Labels common.MapStr
	Meta   common.MapStr
}

// infoMetrics keeps the infoMetrics data
type infoMetrics struct {
	data  []*infoMetricData
	mutex sync.Mutex
}

func (p *prometheus) ReportProcessedMetrics(mapping *MetricsMapping, r mb.ReporterV2) {
	events, err := p.GetProcessedMetrics(mapping)
	if err != nil {
		r.Error(err)
		return
	}
	for _, event := range events {
		r.Event(mb.Event{MetricSetFields: event})
	}
}

type eventsMaps struct {
	events     map[string]common.MapStr
	metricKeys map[string]string
	mapMutex   sync.Mutex
	eventMutex sync.Mutex
}

func (e *eventsMaps) getOrCreateEvent(primaryLabels common.MapStr, secondaryLabels common.MapStr) (bool, common.MapStr) {
	pHash := primaryLabels.String()
	e.mapMutex.Lock()
	defer e.mapMutex.Unlock()
	res, found := e.events[pHash]

	if !found {
		res = primaryLabels
		e.events[pHash] = res
		if len(secondaryLabels) != 0 {
			sHash := secondaryLabels.String()
			e.metricKeys[sHash] = pHash
		}
	}

	return !found, res

}

func (e *eventsMaps) getEvent(primaryLabels common.MapStr, secondaryLabels common.MapStr) common.MapStr {
	pHash := primaryLabels.String()
	e.mapMutex.Lock()
	defer e.mapMutex.Unlock()
	res, found := e.events[pHash]
	if !found {
		if len(secondaryLabels) != 0 {
			sHash := secondaryLabels.String()
			pHash, found = e.metricKeys[sHash]
			if found {
				res, found = e.events[pHash]
			}
		}
	}

	if !found {
		res = nil
	}

	return res
}

func (e *eventsMaps) updateEvent(event common.MapStr, field string, value interface{}, labels common.MapStr) {
	e.eventMutex.Lock()
	defer e.eventMutex.Unlock()
	if field != "" && value != nil {
		event.Put(field, value)
	}
	event.DeepUpdate(labels)
}

func getLabels(metric *dto.Metric) common.MapStr {
	labels := common.MapStr{}
	for _, label := range metric.GetLabel() {
		if label.GetName() != "" && label.GetValue() != "" {
			labels.Put(label.GetName(), label.GetValue())
		}
	}
	return labels
}

func getLabelsByType(mapping *MetricsMapping, metricsLabels common.MapStr) (common.MapStr, common.MapStr, common.MapStr) {
	primaryKeyLabels := common.MapStr{}
	secondaryKeyLabels := common.MapStr{}
	labels := common.MapStr{}
	for k, v := range metricsLabels {
		if l, ok := mapping.Labels[k]; ok {
			if l.IsKey() {
				if len(mapping.KeyLabels.Primary) != 0 {
					if _, found := mapping.KeyLabels.Primary[k]; found {
						primaryKeyLabels.Put(l.GetField(), v)
					}
					if _, found := mapping.KeyLabels.Secondary[k]; found {
						secondaryKeyLabels.Put(l.GetField(), v)
					}
				} else {
					primaryKeyLabels.Put(l.GetField(), v)
				}
			} else {
				labels.Put(l.GetField(), v)
			}
		}
	}

	return primaryKeyLabels, secondaryKeyLabels, labels
}
