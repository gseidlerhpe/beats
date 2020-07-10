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
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
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
	logp.Debug("prometheus", "HTTP client %v", http.GetClient())
	return &prometheus{http}, nil
}

// GetFamilies requests metric families from prometheus endpoint and returns them
func (p *prometheus) GetFamilies(familyPrefix []string) ([]*dto.MetricFamily, error) {
	startNanos := time.Now().UnixNano()
	resp, err := p.FetchResponse()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	endNanos := time.Now().UnixNano()
	logp.Debug("prometheus", "FetchResponse took: %d ms", (endNanos-startNanos)/1000000)

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
			for !done {
				mf := &dto.MetricFamily{}
				decoderMutex.Lock()
				err = decoder.Decode(mf)
				decoderMutex.Unlock()
				if err != nil {
					if err == io.EOF {
						done = true
					}
				} else {
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
			}
			wg.Done()
		}(count)
	}
	logp.Debug("prometheus", "GetFamilies waiting for %d sub fun to complete", count)
	wg.Wait()

	// org code
	// for {
	// 	mf := &dto.MetricFamily{}
	// 	err = decoder.Decode(mf)
	// 	if err != nil {
	// 		if err == io.EOF {
	// 			break
	// 		}
	// 	} else {
	// 		if familyPrefix == nil {
	// 			families = append(families, mf)
	// 		} else {
	// 			for _, prefix := range familyPrefix {
	// 				if strings.HasPrefix(mf.GetName(), prefix) {
	// 					families = append(families, mf)
	// 					break
	// 				}
	// 			}
	// 		}
	// 	}
	// }
	endNanos = time.Now().UnixNano()
	logp.Debug("prometheus", "GetFamilies took: %d ms Got Families %d", (endNanos-startNanos)/1000000, len(families))
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
	// MetricSet Name -- for dbg
	MetricSetName string

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

func isDbgMetrics(mapping *MetricsMapping) (bool, string) {
	//dbgMetrics := map[string]string{"kube_node_info": "state_node", "kube_pod_container_info": "state_container", "kube_pod_info": "state_pod"}
	dbgMetrics := [2]string{"state_container", "state_pod"}
	doDbg := false
	metricSet := ""
	for _, m := range dbgMetrics {
		if mapping.MetricSetName == m {
			return true, m
		}
	}

	return doDbg, metricSet

}

func processMetrics(eventsMap *eventsMaps, mapping *MetricsMapping, families []*dto.MetricFamily) {
	doDbg, metricSet := isDbgMetrics(mapping)

	if doDbg {
		logp.Debug("prometheus", ">>> processMetrics %s", metricSet)
	}

	startNanos := time.Now().UnixNano()
	for _, family := range families {
		var wg sync.WaitGroup

		if _, ok := mapping.InfoMetrics[family.GetName()]; ok {
			continue
		}

		if doDbg {
			logp.Debug("prometheus", "processMetrics %s, family=%s, family metrics=%d", metricSet, family.GetName(), len(family.GetMetric()))
		}

		familyMetrics := family.GetMetric()
		sliceSize := len(familyMetrics)
		if sliceSize > maxMetricsSliceSize {
			sliceSize = maxMetricsSliceSize
		}
		threadCount := 0
		startMetricsNanos := time.Now().UnixNano()
		for i := 0; i < len(familyMetrics); i += sliceSize {
			var slice []*dto.Metric
			if i+sliceSize < len(familyMetrics) {
				slice = familyMetrics[i : i+sliceSize]
			} else {
				slice = familyMetrics[i:]
			}
			wg.Add(1)
			threadCount++
			go func(idMetric int, metrics []*dto.Metric) {
				startProcessMetricsNanos := time.Now().UnixNano()
				if doDbg {
					logp.Debug("prometheus", ">>>>>> processMetrics %s, idMetric=%d, family=%s, metrics=%d", metricSet, idMetric, family.GetName(), len(metrics))
				}

				for _, metric := range metrics {
					m, ok := mapping.Metrics[family.GetName()]

					// Ignore unknown metrics
					if !ok {
						continue
					}

					field := m.GetField()
					value := m.GetValue(metric)

					// Ignore retrieval errors (bad conf)
					if value == nil {
						continue
					}

					// Apply extra options
					allLabels := getLabels(metric)
					for _, option := range m.GetOptions() {
						field, value, allLabels = option.Process(field, value, allLabels)
					}

					primaryKeyLabels, secondaryKeyLabels, labels := getLabelsByType(mapping, allLabels)

					if field != "" {
						// Put it in the event if it's a common metric
						_, event := eventsMap.getOrCreateEvent(primaryKeyLabels, secondaryKeyLabels)
						// if doDbg {
						// 	if isNew {
						// 		logp.Debug("prometheus", "processMetrics %s, idMetric=%d, family=%s, family metric=%v, New event keyLabels=%v, %v event=%v with %s,%v and labels %v", metricSet, idMetric, family.GetName(), metric, primaryKeyLabels, secondaryKeyLabels, event, field, value, labels)
						// 	} else {
						// 		logp.Debug("prometheus", "processMetrics %s, idMetric=%d, family=%s, family metric=%v, Update event keyLabels=%v, %v event=%v with %s,%v and labels %v", metricSet, idMetric, family.GetName(), metric, primaryKeyLabels, secondaryKeyLabels, event, field, value, labels)
						// 	}

						// }
						eventsMap.updateEvent(event, field, value, labels)
					}
				}
				endProcessMetricsNanos := time.Now().UnixNano()
				if doDbg {
					logp.Debug("prometheus", "<<<<<< processMetrics %s, idMetric=%d, family=%s, metrics=%d took: %d ms, events=%d", metricSet, idMetric, family.GetName(), len(metrics), (endProcessMetricsNanos-startProcessMetricsNanos)/1000000, len(eventsMap.events))
				}
				wg.Done()
			}(i, slice)
		}
		if doDbg {
			logp.Debug("prometheus", "processMetrics %s, family=%s, family metrics=%d waiting for %d threads to complete...", metricSet, family.GetName(), len(family.GetMetric()), threadCount)
		}
		wg.Wait()
		endMetricsNanos := time.Now().UnixNano()
		if doDbg {
			logp.Debug("prometheus", "processMetrics %s, family=%s, family metrics=%d %d threads took %d ms to complete", metricSet, family.GetName(), len(family.GetMetric()), threadCount, (endMetricsNanos-startMetricsNanos)/1000000)
		}

	}

	endNanos := time.Now().UnixNano()
	if doDbg {
		logp.Debug("prometheus", "<<< processMetrics %s took: %d ms, events %d", metricSet, (endNanos-startNanos)/1000000, len(eventsMap.events))
	}

}

func processInfoMetrics(eventsMap *eventsMaps, mapping *MetricsMapping, families []*dto.MetricFamily) {
	infoMetrics := []*infoMetricData{}
	var infoMetricsMutex = &sync.Mutex{}
	doDbg, metricSet := isDbgMetrics(mapping)

	if doDbg {
		logp.Debug("prometheus", ">>> processInfoMetrics %s", metricSet)
	}
	startNanos := time.Now().UnixNano()
	for _, family := range families {
		var wg sync.WaitGroup

		if _, ok := mapping.Metrics[family.GetName()]; ok {
			continue
		}

		if doDbg {
			logp.Debug("prometheus", "processInfoMetrics %s, family=%s, family metrics=%d", metricSet, family.GetName(), len(family.GetMetric()))
		}

		familyMetrics := family.GetMetric()
		sliceSize := len(familyMetrics)
		if sliceSize > maxMetricsSliceSize {
			sliceSize = maxMetricsSliceSize
		}
		threadCount := 0
		startMetricsNanos := time.Now().UnixNano()
		for i := 0; i < len(familyMetrics); i += sliceSize {
			var slice []*dto.Metric
			if i+sliceSize < len(familyMetrics) {
				slice = familyMetrics[i : i+sliceSize]
			} else {
				slice = familyMetrics[i:]
			}
			wg.Add(1)
			threadCount++
			go func(idMetric int, metrics []*dto.Metric) {
				startProcessMetricsNanos := time.Now().UnixNano()
				if doDbg {
					logp.Debug("prometheus", ">>>>>> processInfoMetrics %s, idMetric=%d, family=%s, metrics=%d", metricSet, idMetric, family.GetName(), len(metrics))
				}
				for _, metric := range metrics {
					m, ok := mapping.InfoMetrics[family.GetName()]
					// Ignore unknown metrics
					if !ok {
						continue
					}

					field := m.GetField()
					value := m.GetValue(metric)

					// Ignore retrieval errors (bad conf)
					if value == nil {
						continue
					}

					// Apply extra options
					allLabels := getLabels(metric)
					for _, option := range m.GetOptions() {
						field, value, allLabels = option.Process(field, value, allLabels)
					}

					primaryKeyLabels, secondaryKeyLabels, labels := getLabelsByType(mapping, allLabels)
					labels.DeepUpdate(primaryKeyLabels)
					event := eventsMap.getEvent(primaryKeyLabels, secondaryKeyLabels)
					if event != nil {
						eventsMap.updateEvent(event, "", nil, labels)
					} else {
						// No matching event found!
						// Add metrics that have additional labels for later processing only
						if primaryKeyLabels.String() != labels.String() {
							infoMetricsMutex.Lock()
							infoMetrics = append(infoMetrics, &infoMetricData{
								Labels: primaryKeyLabels,
								Meta:   labels,
							})
							infoMetricsMutex.Unlock()
						}
					}
				}
				endProcessMetricsNanos := time.Now().UnixNano()
				if doDbg {
					logp.Debug("prometheus", "<<<<<< processInfoMetrics %s, idMetric=%d, family=%s, metrics=%d took: %d ms, events=%d", metricSet, idMetric, family.GetName(), len(metrics), (endProcessMetricsNanos-startProcessMetricsNanos)/1000000, len(eventsMap.events))
				}
				wg.Done()
			}(i, slice)
		}
		if doDbg {
			logp.Debug("prometheus", "processInfoMetrics %s, family=%s, family metrics=%d waiting for %d threads to complete...", metricSet, family.GetName(), len(family.GetMetric()), threadCount)
		}
		wg.Wait()
		endMetricsNanos := time.Now().UnixNano()
		if doDbg {
			logp.Debug("prometheus", "processInfoMetrics %s, family=%s, family metrics=%d %d threads took %d ms to complete", metricSet, family.GetName(), len(family.GetMetric()), threadCount, (endMetricsNanos-startMetricsNanos)/1000000)
		}

	}

	endNanos := time.Now().UnixNano()
	cpNanos := endNanos
	if doDbg {
		logp.Debug("prometheus", "processInfoMetrics %s took: %d ms, families %d, events %d, infoMetrics %d", metricSet, (endNanos-startNanos)/1000000, len(families), len(eventsMap.events), len(infoMetrics))
	}

	cpNanos = endNanos
	// fill info from infoMetrics
	for _, info := range infoMetrics {
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
	endNanos = time.Now().UnixNano()
	if doDbg {
		logp.Debug("prometheus", "processInfoMetrics %s took: %d ms to fill %d infoMetrics into %d events", metricSet, (endNanos-cpNanos)/1000000, len(infoMetrics), len(eventsMap.events))
		logp.Debug("prometheus", "<<< processInfoMetrics %s took: %d ms, events %d", metricSet, (endNanos-startNanos)/1000000, len(eventsMap.events))
	}

}

func (p *prometheus) GetProcessedMetrics(mapping *MetricsMapping) ([]common.MapStr, error) {
	eventsMap := eventsMaps{
		events:     map[string]common.MapStr{},
		metricKeys: map[string]string{},
		mapMutex:   sync.Mutex{},
		eventMutex: sync.Mutex{},
	}
	doDbg, metricSet := isDbgMetrics(mapping)
	if doDbg {
		logp.Debug("prometheus", ">>> GetProcessedMetrics %s", metricSet)
	}

	startNanos := time.Now().UnixNano()
	families, err := p.GetFamilies(mapping.FamilyPrefix)
	if err != nil {
		return nil, err
	}

	// first process non-InfoMetrics
	processMetrics(&eventsMap, mapping, families)
	endNanos := time.Now().UnixNano()
	if doDbg {
		logp.Debug("prometheus", "  GetProcessedMetrics %s processMetrics took: %d ms, events %d", metricSet, (endNanos-startNanos)/1000000, len(eventsMap.events))
	}
	procNanos := time.Now().UnixNano()
	// Now process infoMetrics and fill in additional info
	processInfoMetrics(&eventsMap, mapping, families)
	endNanos = time.Now().UnixNano()
	if doDbg {
		logp.Debug("prometheus", "  GetProcessedMetrics %s processInfoMetrics took: %d ms, events %d", metricSet, (endNanos-procNanos)/1000000, len(eventsMap.events))
	}

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

	endNanos = time.Now().UnixNano()
	if doDbg {
		logp.Debug("prometheus", "<<< GetProcessedMetrics %s took: %d ms, events %d", metricSet, (endNanos-startNanos)/1000000, len(events))
	}
	return events, nil

}

// infoMetricData keeps data about an infoMetric
type infoMetricData struct {
	Labels common.MapStr
	Meta   common.MapStr
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
	res, found := e.events[pHash]

	if !found {
		res = primaryLabels
		e.events[pHash] = res
		if len(secondaryLabels) != 0 {
			sHash := secondaryLabels.String()
			e.metricKeys[sHash] = pHash
		}
	}

	defer e.mapMutex.Unlock()
	return !found, res

}

func (e *eventsMaps) getEvent(primaryLabels common.MapStr, secondaryLabels common.MapStr) common.MapStr {
	pHash := primaryLabels.String()
	e.mapMutex.Lock()
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

	defer e.mapMutex.Unlock()
	return res
}

func (e *eventsMaps) updateEvent(event common.MapStr, field string, value interface{}, labels common.MapStr) {
	e.eventMutex.Lock()
	if field != "" && value != nil {
		event.Put(field, value)
	}
	event.DeepUpdate(labels)
	e.eventMutex.Unlock()
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
