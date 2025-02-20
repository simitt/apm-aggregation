// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package aggregators

import (
	"context"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/cockroachdb/pebble"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.elastic.co/apm/module/apmotel/v2"
	"go.elastic.co/apm/v2"
	"go.elastic.co/apm/v2/apmtest"
	apmmodel "go.elastic.co/apm/v2/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/elastic/apm-aggregation/aggregationpb"
	"github.com/elastic/apm-aggregation/aggregators/internal/hdrhistogram"
	"github.com/elastic/apm-data/model/modelpb"
)

func TestNew(t *testing.T) {
	agg, err := New()
	assert.NoError(t, err)
	assert.NotNil(t, agg)
}

func TestAggregateBatch(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
	)
	gatherer, err := apmotel.NewGatherer()
	require.NoError(t, err)
	mp := metric.NewMeterProvider(metric.WithReader(gatherer))

	cmID := EncodeToCombinedMetricsKeyID(t, "ab01")
	eventDuration := 100 * time.Millisecond
	dssDuration := 10 * time.Millisecond
	uniqueEventCount := 100 // for each of txns and spans
	uniqueServices := 10
	repCount := 5
	ts := time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC)
	batch := make(modelpb.Batch, 0, uniqueEventCount*repCount*2)
	// Distribute the total unique transaction count amongst the total
	// unique services uniformly.
	for i := 0; i < uniqueEventCount*repCount; i++ {
		batch = append(batch, &modelpb.APMEvent{
			Event: &modelpb.Event{
				Outcome:  "success",
				Duration: durationpb.New(eventDuration),
				Received: timestamppb.New(ts),
			},
			Transaction: &modelpb.Transaction{
				Name:                fmt.Sprintf("foo%d", i%uniqueEventCount),
				Type:                fmt.Sprintf("txtype%d", i%uniqueEventCount),
				RepresentativeCount: 1,
				DroppedSpansStats: []*modelpb.DroppedSpanStats{
					{
						DestinationServiceResource: fmt.Sprintf("dropped_dest_resource%d", i%uniqueEventCount),
						Outcome:                    "success",
						Duration: &modelpb.AggregatedDuration{
							Count: 1,
							Sum:   durationpb.New(dssDuration),
						},
					},
				},
			},
			Service: &modelpb.Service{Name: fmt.Sprintf("svc%d", i%uniqueServices)},
		})
		batch = append(batch, &modelpb.APMEvent{
			Event: &modelpb.Event{
				Duration: durationpb.New(eventDuration),
				Received: timestamppb.New(ts),
			},
			Span: &modelpb.Span{
				Name:                fmt.Sprintf("bar%d", i%uniqueEventCount),
				Type:                "type",
				RepresentativeCount: 1,
				DestinationService: &modelpb.DestinationService{
					Resource: "test_dest",
				},
			},
			Service: &modelpb.Service{Name: fmt.Sprintf("svc%d", i%uniqueServices)},
		})
	}

	out := make(chan *aggregationpb.CombinedMetrics, 1)
	aggIvl := time.Minute
	agg, err := New(
		WithDataDir(t.TempDir()),
		WithLimits(Limits{
			MaxSpanGroups:                         1000,
			MaxSpanGroupsPerService:               100,
			MaxTransactionGroups:                  100,
			MaxTransactionGroupsPerService:        10,
			MaxServiceTransactionGroups:           100,
			MaxServiceTransactionGroupsPerService: 10,
			MaxServices:                           10,
			MaxServiceInstanceGroupsPerService:    10,
		}),
		WithProcessor(combinedMetricsProcessor(out)),
		WithAggregationIntervals([]time.Duration{aggIvl}),
		WithHarvestDelay(time.Hour), // disable auto harvest
		WithTracer(tp.Tracer("test")),
		WithMeter(mp.Meter("test")),
		WithCombinedMetricsIDToKVs(func(id [16]byte) []attribute.KeyValue {
			return []attribute.KeyValue{attribute.String("id_key", string(id[:]))}
		}),
	)
	require.NoError(t, err)

	require.NoError(t, agg.AggregateBatch(context.Background(), cmID, &batch))
	require.NoError(t, agg.Close(context.Background()))
	var cm *aggregationpb.CombinedMetrics
	select {
	case cm = <-out:
	default:
		t.Error("failed to get aggregated metrics")
		t.FailNow()
	}

	var span tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "AggregateBatch" {
			span = s
		}
	}
	assert.NotNil(t, span)

	expectedCombinedMetrics := NewTestCombinedMetrics(
		WithEventsTotal(float64(len(batch))),
		WithYoungestEventTimestamp(ts),
	)
	expectedMeasurements := []apmmodel.Metrics{
		{
			Samples: map[string]apmmodel.Metric{
				"aggregator.requests.total": {Value: 1},
				"aggregator.bytes.ingested": {Value: 138250},
			},
			Labels: apmmodel.StringMap{
				apmmodel.StringMapItem{Key: "id_key", Value: string(cmID[:])},
			},
		},
		{
			Samples: map[string]apmmodel.Metric{
				"aggregator.events.total":     {Value: float64(len(batch))},
				"aggregator.events.processed": {Value: float64(len(batch))},
				"events.processing-delay":     {Type: "histogram", Counts: []uint64{1}, Values: []float64{0}},
				"events.queued-delay":         {Type: "histogram", Counts: []uint64{1}, Values: []float64{0}},
			},
			Labels: apmmodel.StringMap{
				apmmodel.StringMapItem{Key: aggregationIvlKey, Value: formatDuration(aggIvl)},
				apmmodel.StringMapItem{Key: "id_key", Value: string(cmID[:])},
			},
		},
	}
	sik := serviceInstanceAggregationKey{GlobalLabelsStr: ""}
	for i := 0; i < uniqueEventCount*repCount; i++ {
		svcKey := serviceAggregationKey{
			Timestamp:   time.Unix(0, 0).UTC(),
			ServiceName: fmt.Sprintf("svc%d", i%uniqueServices),
		}
		txKey := transactionAggregationKey{
			TraceRoot:       true,
			TransactionName: fmt.Sprintf("foo%d", i%uniqueEventCount),
			TransactionType: fmt.Sprintf("txtype%d", i%uniqueEventCount),
			EventOutcome:    "success",
		}
		stxKey := serviceTransactionAggregationKey{
			TransactionType: fmt.Sprintf("txtype%d", i%uniqueEventCount),
		}
		spanKey := spanAggregationKey{
			SpanName: fmt.Sprintf("bar%d", i%uniqueEventCount),
			Resource: "test_dest",
		}
		dssKey := spanAggregationKey{
			SpanName: "",
			Resource: fmt.Sprintf("dropped_dest_resource%d", i%uniqueEventCount),
			Outcome:  "success",
		}
		expectedCombinedMetrics.
			AddServiceMetrics(svcKey).
			AddServiceInstanceMetrics(sik).
			AddTransaction(txKey, WithTransactionDuration(eventDuration)).
			AddServiceTransaction(stxKey, WithTransactionDuration(eventDuration)).
			AddSpan(spanKey, WithSpanDuration(eventDuration)).
			AddSpan(dssKey, WithSpanDuration(dssDuration))
	}
	assert.Empty(t, cmp.Diff(
		expectedCombinedMetrics.GetProto(), cm,
		append(combinedMetricsSliceSorters,
			cmpopts.EquateEmpty(),
			cmpopts.EquateApprox(0, 0.01),
			cmp.Comparer(func(a, b hdrhistogram.HybridCountsRep) bool {
				return a.Equal(&b)
			}),
			protocmp.Transform(),
		)...,
	))
	assert.Empty(t, cmp.Diff(
		expectedMeasurements,
		gatherMetrics(
			gatherer,
			withIgnoreMetricPrefix("pebble."),
			withZeroHistogramValues(true),
		),
		cmpopts.IgnoreUnexported(apmmodel.Time{}),
		cmpopts.EquateApprox(0, 0.01),
	))
}

func TestAggregateSpanMetrics(t *testing.T) {
	type input struct {
		serviceName         string
		agentName           string
		destination         string
		targetType          string
		targetName          string
		outcome             string
		representativeCount float64
	}

	destinationX := "destination-X"
	destinationZ := "destination-Z"
	trgTypeX := "trg-type-X"
	trgNameX := "trg-name-X"
	trgTypeZ := "trg-type-Z"
	trgNameZ := "trg-name-Z"
	defaultLabels := modelpb.Labels{
		"department_name": &modelpb.LabelValue{Global: true, Value: "apm"},
		"organization":    &modelpb.LabelValue{Global: true, Value: "observability"},
		"company":         &modelpb.LabelValue{Global: true, Value: "elastic"},
	}
	defaultNumericLabels := modelpb.NumericLabels{
		"user_id":     &modelpb.NumericLabelValue{Global: true, Value: 100},
		"cost_center": &modelpb.NumericLabelValue{Global: true, Value: 10},
	}

	for _, tt := range []struct {
		name              string
		inputs            []input
		getExpectedEvents func(time.Time, time.Duration, time.Duration, int) []*modelpb.APMEvent
	}{
		{
			name: "with destination and service targets",
			inputs: []input{
				{serviceName: "service-A", agentName: "java", destination: destinationZ, targetType: trgTypeZ, targetName: trgNameZ, outcome: "success", representativeCount: 2},
				{serviceName: "service-A", agentName: "java", destination: destinationX, targetType: trgTypeX, targetName: trgNameX, outcome: "success", representativeCount: 1},
				{serviceName: "service-B", agentName: "python", destination: destinationZ, targetType: trgTypeZ, targetName: trgNameZ, outcome: "success", representativeCount: 1},
				{serviceName: "service-A", agentName: "java", destination: destinationZ, targetType: trgTypeZ, targetName: trgNameZ, outcome: "success", representativeCount: 1},
				{serviceName: "service-A", agentName: "java", destination: destinationZ, targetType: trgTypeZ, targetName: trgNameZ, outcome: "success", representativeCount: 0},
				{serviceName: "service-A", agentName: "java", destination: destinationZ, targetType: trgTypeZ, targetName: trgNameZ, outcome: "failure", representativeCount: 1},
			},
			getExpectedEvents: func(ts time.Time, duration, ivl time.Duration, count int) []*modelpb.APMEvent {
				return []*modelpb.APMEvent{
					{
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
						},
						Metricset: &modelpb.Metricset{
							Name:     "service_summary",
							Interval: formatDuration(ivl),
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "python"},
						Service: &modelpb.Service{
							Name: "service-B",
						},
						Metricset: &modelpb.Metricset{
							Name:     "service_summary",
							Interval: formatDuration(ivl),
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
							Target: &modelpb.ServiceTarget{
								Type: trgTypeX,
								Name: trgNameX,
							},
						},
						Event: &modelpb.Event{Outcome: "success"},
						Metricset: &modelpb.Metricset{
							Name:     "service_destination",
							Interval: formatDuration(ivl),
							DocCount: uint64(count),
						},
						Span: &modelpb.Span{
							Name: "service-A:" + destinationX,
							DestinationService: &modelpb.DestinationService{
								Resource: destinationX,
								ResponseTime: &modelpb.AggregatedDuration{
									Count: uint64(count),
									Sum:   durationpb.New(time.Duration(count) * duration),
								},
							},
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
							Target: &modelpb.ServiceTarget{
								Type: trgTypeZ,
								Name: trgNameZ,
							},
						},
						Event: &modelpb.Event{Outcome: "failure"},
						Metricset: &modelpb.Metricset{
							Name:     "service_destination",
							Interval: formatDuration(ivl),
							DocCount: uint64(count),
						},
						Span: &modelpb.Span{
							Name: "service-A:" + destinationZ,
							DestinationService: &modelpb.DestinationService{
								Resource: destinationZ,
								ResponseTime: &modelpb.AggregatedDuration{
									Count: uint64(count),
									Sum:   durationpb.New(time.Duration(count) * duration),
								},
							},
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
							Target: &modelpb.ServiceTarget{
								Type: trgTypeZ,
								Name: trgNameZ,
							},
						},
						Event: &modelpb.Event{Outcome: "success"},
						Metricset: &modelpb.Metricset{
							Name:     "service_destination",
							Interval: formatDuration(ivl),
							DocCount: uint64(3 * count),
						},
						Span: &modelpb.Span{
							Name: "service-A:" + destinationZ,
							DestinationService: &modelpb.DestinationService{
								Resource: destinationZ,
								ResponseTime: &modelpb.AggregatedDuration{
									Count: uint64(3 * count),
									Sum:   durationpb.New(time.Duration(3*count) * duration),
								},
							},
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "python"},
						Service: &modelpb.Service{
							Name: "service-B",
							Target: &modelpb.ServiceTarget{
								Type: trgTypeZ,
								Name: trgNameZ,
							},
						},
						Event: &modelpb.Event{Outcome: "success"},
						Metricset: &modelpb.Metricset{
							Name:     "service_destination",
							Interval: formatDuration(ivl),
							DocCount: uint64(count),
						},
						Span: &modelpb.Span{
							Name: "service-B:" + destinationZ,
							DestinationService: &modelpb.DestinationService{
								Resource: destinationZ,
								ResponseTime: &modelpb.AggregatedDuration{
									Count: uint64(count),
									Sum:   durationpb.New(time.Duration(count) * duration),
								},
							},
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					},
				}
			},
		}, {
			name: "with_no_destination_and_no_service_target",
			inputs: []input{
				{serviceName: "service-A", agentName: "java", outcome: "success", representativeCount: 1},
			},
			getExpectedEvents: func(_ time.Time, _, _ time.Duration, _ int) []*modelpb.APMEvent {
				return nil
			},
		}, {
			name: "with no destination and a service target",
			inputs: []input{
				{serviceName: "service-A", agentName: "java", targetType: trgTypeZ, targetName: trgNameZ, outcome: "success", representativeCount: 1},
			},
			getExpectedEvents: func(ts time.Time, duration, ivl time.Duration, count int) []*modelpb.APMEvent {
				return []*modelpb.APMEvent{
					{
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
						},
						Metricset: &modelpb.Metricset{
							Name:     "service_summary",
							Interval: formatDuration(ivl),
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
							Target: &modelpb.ServiceTarget{
								Type: trgTypeZ,
								Name: trgNameZ,
							},
						},
						Event: &modelpb.Event{Outcome: "success"},
						Metricset: &modelpb.Metricset{
							Name:     "service_destination",
							Interval: formatDuration(ivl),
							DocCount: uint64(count),
						},
						Span: &modelpb.Span{
							Name: "service-A:",
							DestinationService: &modelpb.DestinationService{
								ResponseTime: &modelpb.AggregatedDuration{
									Count: uint64(count),
									Sum:   durationpb.New(time.Duration(count) * duration),
								},
							},
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					},
				}
			},
		}, {
			name: "with a destination and no service target",
			inputs: []input{
				{serviceName: "service-A", agentName: "java", destination: destinationZ, outcome: "success", representativeCount: 1},
			},
			getExpectedEvents: func(ts time.Time, duration, ivl time.Duration, count int) []*modelpb.APMEvent {
				return []*modelpb.APMEvent{
					{
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
						},
						Metricset: &modelpb.Metricset{
							Name:     "service_summary",
							Interval: formatDuration(ivl),
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					}, {
						Timestamp: timestamppb.New(ts.Truncate(ivl)),
						Agent:     &modelpb.Agent{Name: "java"},
						Service: &modelpb.Service{
							Name: "service-A",
						},
						Event: &modelpb.Event{Outcome: "success"},
						Metricset: &modelpb.Metricset{
							Name:     "service_destination",
							Interval: formatDuration(ivl),
							DocCount: uint64(count),
						},
						Span: &modelpb.Span{
							Name: "service-A:" + destinationZ,
							DestinationService: &modelpb.DestinationService{
								Resource: destinationZ,
								ResponseTime: &modelpb.AggregatedDuration{
									Count: uint64(count),
									Sum:   durationpb.New(time.Duration(count) * duration),
								},
							},
						},
						Labels:        defaultLabels,
						NumericLabels: defaultNumericLabels,
					},
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var actualEvents []*modelpb.APMEvent
			aggregationIvls := []time.Duration{time.Minute, 10 * time.Minute, time.Hour}
			agg, err := New(
				WithLimits(Limits{
					MaxSpanGroups:                         1000,
					MaxSpanGroupsPerService:               100,
					MaxTransactionGroups:                  100,
					MaxTransactionGroupsPerService:        10,
					MaxServiceTransactionGroups:           100,
					MaxServiceTransactionGroupsPerService: 10,
					MaxServices:                           10,
					MaxServiceInstanceGroupsPerService:    10,
				}),
				WithAggregationIntervals(aggregationIvls),
				WithProcessor(sliceProcessor(&actualEvents)),
				WithDataDir(t.TempDir()),
			)
			require.NoError(t, err)

			count := 100
			now := time.Now()
			duration := 100 * time.Millisecond
			for _, in := range tt.inputs {
				span := makeSpan(
					now,
					in.serviceName,
					in.agentName,
					in.destination,
					in.targetType,
					in.targetName,
					in.outcome,
					duration,
					in.representativeCount,
					defaultLabels,
					defaultNumericLabels,
				)
				for i := 0; i < count; i++ {
					err := agg.AggregateBatch(
						context.Background(),
						EncodeToCombinedMetricsKeyID(t, "ab01"),
						&modelpb.Batch{span},
					)
					require.NoError(t, err)
				}
			}
			require.NoError(t, agg.Close(context.Background()))
			var expectedEvents []*modelpb.APMEvent
			for _, ivl := range aggregationIvls {
				expectedEvents = append(expectedEvents, tt.getExpectedEvents(now, duration, ivl, count)...)
			}
			sortKey := func(e *modelpb.APMEvent) string {
				var sb strings.Builder
				sb.WriteString(e.GetService().GetName())
				sb.WriteString(e.GetAgent().GetName())
				sb.WriteString(e.GetMetricset().GetName())
				sb.WriteString(e.GetMetricset().GetInterval())
				destSvc := e.GetSpan().GetDestinationService()
				if destSvc != nil {
					sb.WriteString(destSvc.GetResource())
				}
				target := e.GetService().GetTarget()
				if target != nil {
					sb.WriteString(target.GetName())
					sb.WriteString(target.GetType())
				}
				sb.WriteString(e.GetEvent().GetOutcome())
				return sb.String()
			}
			sort.Slice(expectedEvents, func(i, j int) bool {
				return sortKey(expectedEvents[i]) < sortKey(expectedEvents[j])
			})
			sort.Slice(actualEvents, func(i, j int) bool {
				return sortKey(actualEvents[i]) < sortKey(actualEvents[j])
			})
			assert.Empty(t, cmp.Diff(
				expectedEvents, actualEvents,
				cmpopts.EquateEmpty(),
				cmpopts.IgnoreTypes(netip.Addr{}),
				protocmp.Transform(),
			))
		})
	}
}

func TestCombinedMetricsKeyOrdered(t *testing.T) {
	// To Allow for retrieving combined metrics by time range, the metrics should
	// be ordered by processing time.
	ts := time.Now().Add(-time.Hour)
	ivl := time.Minute

	cmID := EncodeToCombinedMetricsKeyID(t, "ab01")
	before := CombinedMetricsKey{
		ProcessingTime: ts.Truncate(time.Minute),
		Interval:       ivl,
		ID:             cmID,
	}
	beforeBytes := make([]byte, CombinedMetricsKeyEncodedSize)
	afterBytes := make([]byte, CombinedMetricsKeyEncodedSize)

	for i := 0; i < 10; i++ {
		ts = ts.Add(time.Minute)
		cmID = EncodeToCombinedMetricsKeyID(t, fmt.Sprintf("ab%02d", rand.Intn(100)))
		after := CombinedMetricsKey{
			ProcessingTime: ts.Truncate(time.Minute),
			Interval:       ivl,
			// combined metrics ID shouldn't matter. Keep length to be
			// 5 to ensure it is within expected bounds of the
			// sized buffer.
			ID: cmID,
		}
		require.NoError(t, after.MarshalBinaryToSizedBuffer(afterBytes))
		require.NoError(t, before.MarshalBinaryToSizedBuffer(beforeBytes))

		// before should always come first
		assert.Equal(t, -1, pebble.DefaultComparer.Compare(beforeBytes, afterBytes))

		before = after
	}
}

// Keys should be ordered such that all the partitions for a specific ID is listed
// before any other combined metrics ID.
func TestCombinedMetricsKeyOrderedByProjectID(t *testing.T) {
	// To Allow for retrieving combined metrics by time range, the metrics should
	// be ordered by processing time.
	ts := time.Now().Add(-time.Hour)
	ivl := time.Minute

	keyTemplate := CombinedMetricsKey{
		ProcessingTime: ts.Truncate(time.Minute),
		Interval:       ivl,
	}
	cmCount := 1000
	pidCount := 500
	keys := make([]CombinedMetricsKey, 0, cmCount*pidCount)

	for i := 0; i < cmCount; i++ {
		cmID := EncodeToCombinedMetricsKeyID(t, fmt.Sprintf("ab%06d", i))
		for k := 0; k < pidCount; k++ {
			key := keyTemplate
			key.PartitionID = uint16(k)
			key.ID = cmID
			keys = append(keys, key)
		}
	}

	before := keys[0]
	beforeBytes := make([]byte, CombinedMetricsKeyEncodedSize)
	afterBytes := make([]byte, CombinedMetricsKeyEncodedSize)

	for i := 1; i < len(keys); i++ {
		ts = ts.Add(time.Minute)
		after := keys[i]
		require.NoError(t, after.MarshalBinaryToSizedBuffer(afterBytes))
		require.NoError(t, before.MarshalBinaryToSizedBuffer(beforeBytes))

		// before should always come first
		if !assert.Equal(
			t, -1,
			pebble.DefaultComparer.Compare(beforeBytes, afterBytes),
			fmt.Sprintf("(%s, %d) should come before (%s, %d)", before.ID, before.PartitionID, after.ID, after.PartitionID),
		) {
			assert.FailNow(t, "keys not in expected order")
		}

		before = after
	}
}

func TestHarvest(t *testing.T) {
	cmCount := 5
	ivls := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	m := make(map[time.Duration]map[[16]byte]bool)
	processorDone := make(chan struct{})
	processor := func(
		_ context.Context,
		cmk CombinedMetricsKey,
		_ *aggregationpb.CombinedMetrics,
		ivl time.Duration,
	) error {
		cmMap, ok := m[ivl]
		if !ok {
			m[ivl] = make(map[[16]byte]bool)
			cmMap = m[ivl]
		}
		// For each unique interval, we should only have a single combined metrics ID
		if _, ok := cmMap[cmk.ID]; ok {
			assert.FailNow(t, "duplicate combined metrics ID found")
		}
		cmMap[cmk.ID] = true
		// For successful harvest, all combined metrics IDs foreach interval should be
		// harvested
		if len(m) == len(ivls) {
			var remaining bool
			for k := range m {
				if len(m[k]) != cmCount {
					remaining = true
				}
			}
			if !remaining {
				close(processorDone)
			}
		}
		return nil
	}
	gatherer, err := apmotel.NewGatherer()
	require.NoError(t, err)

	agg, err := New(
		WithDataDir(t.TempDir()),
		WithLimits(Limits{
			MaxSpanGroups:                         1000,
			MaxTransactionGroups:                  100,
			MaxTransactionGroupsPerService:        10,
			MaxServiceTransactionGroups:           100,
			MaxServiceTransactionGroupsPerService: 10,
			MaxServices:                           10,
			MaxServiceInstanceGroupsPerService:    10,
		}),
		WithProcessor(processor),
		WithAggregationIntervals(ivls),
		WithMeter(metric.NewMeterProvider(metric.WithReader(gatherer)).Meter("test")),
		WithCombinedMetricsIDToKVs(func(id [16]byte) []attribute.KeyValue {
			return []attribute.KeyValue{attribute.String("id_key", string(id[:]))}
		}),
	)
	require.NoError(t, err)
	go func() {
		agg.Run(context.Background())
	}()
	t.Cleanup(func() {
		agg.Close(context.Background())
	})

	var batch modelpb.Batch
	batch = append(batch, &modelpb.APMEvent{
		Transaction: &modelpb.Transaction{
			Name:                "txn",
			Type:                "type",
			RepresentativeCount: 1,
		},
	})
	expectedMeasurements := make([]apmmodel.Metrics, 0, cmCount+(cmCount*len(ivls)))
	for i := 0; i < cmCount; i++ {
		cmID := EncodeToCombinedMetricsKeyID(t, fmt.Sprintf("ab%2d", i))
		require.NoError(t, agg.AggregateBatch(context.Background(), cmID, &batch))
		expectedMeasurements = append(expectedMeasurements, apmmodel.Metrics{
			Samples: map[string]apmmodel.Metric{
				"aggregator.requests.total": {Value: 1},
				"aggregator.bytes.ingested": {Value: 270},
			},
			Labels: apmmodel.StringMap{
				apmmodel.StringMapItem{Key: "id_key", Value: string(cmID[:])},
			},
		})
		for _, ivl := range ivls {
			expectedMeasurements = append(expectedMeasurements, apmmodel.Metrics{
				Samples: map[string]apmmodel.Metric{
					"aggregator.events.total":     {Value: float64(len(batch))},
					"aggregator.events.processed": {Value: float64(len(batch))},
					"events.processing-delay":     {Type: "histogram", Counts: []uint64{1}, Values: []float64{0}},
					"events.queued-delay":         {Type: "histogram", Counts: []uint64{1}, Values: []float64{0}},
				},
				Labels: apmmodel.StringMap{
					apmmodel.StringMapItem{Key: aggregationIvlKey, Value: ivl.String()},
					apmmodel.StringMapItem{Key: "id_key", Value: string(cmID[:])},
				},
			})
		}
	}

	// The test is designed to timeout if it fails. The test asserts most of the
	// logic in processor. If all expected metrics are harvested then the
	// processor broadcasts this by closing the processorDone channel and we call
	// it a success. If the harvest hasn't finished then the test times out and
	// we call it a failure. Due to the nature of how the aggregator works, it is
	// possible that this test becomes flaky if there is a bug.
	select {
	case <-processorDone:
	case <-time.After(8 * time.Second):
		t.Fatal("harvest didn't finish within expected time")
	}
	assert.Empty(t, cmp.Diff(
		expectedMeasurements,
		gatherMetrics(
			gatherer,
			withIgnoreMetricPrefix("pebble."),
			withZeroHistogramValues(true),
		),
		cmpopts.IgnoreUnexported(apmmodel.Time{}),
		cmpopts.SortSlices(func(a, b apmmodel.Metrics) bool {
			if len(a.Labels) != len(b.Labels) {
				return len(a.Labels) < len(b.Labels)
			}
			for i := 0; i < len(a.Labels); i++ {
				// assuming keys are ordered
				if a.Labels[i].Value != b.Labels[i].Value {
					return a.Labels[i].Value < b.Labels[i].Value
				}
			}
			return false
		}),
	))
}

func TestAggregateAndHarvest(t *testing.T) {
	txnDuration := 100 * time.Millisecond
	batch := modelpb.Batch{
		{
			Event: &modelpb.Event{
				Outcome:  "success",
				Duration: durationpb.New(txnDuration),
			},
			Transaction: &modelpb.Transaction{
				Name:                "foo",
				Type:                "txtype",
				RepresentativeCount: 1,
			},
			Service: &modelpb.Service{Name: "svc"},
			Labels: modelpb.Labels{
				"department_name": &modelpb.LabelValue{Global: true, Value: "apm"},
				"organization":    &modelpb.LabelValue{Global: true, Value: "observability"},
				"company":         &modelpb.LabelValue{Global: true, Value: "elastic"},
				"mylabel":         &modelpb.LabelValue{Global: false, Value: "myvalue"},
			},
			NumericLabels: modelpb.NumericLabels{
				"user_id":        &modelpb.NumericLabelValue{Global: true, Value: 100},
				"cost_center":    &modelpb.NumericLabelValue{Global: true, Value: 10},
				"mynumericlabel": &modelpb.NumericLabelValue{Global: false, Value: 1},
			},
		},
	}
	var events []*modelpb.APMEvent
	agg, err := New(
		WithDataDir(t.TempDir()),
		WithLimits(Limits{
			MaxSpanGroups:                         1000,
			MaxSpanGroupsPerService:               100,
			MaxTransactionGroups:                  100,
			MaxTransactionGroupsPerService:        10,
			MaxServiceTransactionGroups:           100,
			MaxServiceTransactionGroupsPerService: 10,
			MaxServices:                           10,
			MaxServiceInstanceGroupsPerService:    10,
		}),
		WithProcessor(sliceProcessor(&events)),
		WithAggregationIntervals([]time.Duration{time.Second}),
	)
	require.NoError(t, err)
	require.NoError(t, agg.AggregateBatch(
		context.Background(),
		EncodeToCombinedMetricsKeyID(t, "ab01"),
		&batch,
	))
	require.NoError(t, agg.Close(context.Background()))

	expected := []*modelpb.APMEvent{
		{
			Timestamp: timestamppb.New(time.Unix(0, 0).UTC()),
			Event: &modelpb.Event{
				SuccessCount: &modelpb.SummaryMetric{
					Count: 1,
					Sum:   1,
				},
				Outcome: "success",
			},
			Transaction: &modelpb.Transaction{
				Name: "foo",
				Type: "txtype",
				Root: true,
				DurationSummary: &modelpb.SummaryMetric{
					Count: 1,
					Sum:   100351, // Estimate from histogram
				},
				DurationHistogram: &modelpb.Histogram{
					Values: []float64{100351},
					Counts: []uint64{1},
				},
			},
			Service: &modelpb.Service{
				Name: "svc",
			},
			Labels: modelpb.Labels{
				"department_name": &modelpb.LabelValue{Global: true, Value: "apm"},
				"organization":    &modelpb.LabelValue{Global: true, Value: "observability"},
				"company":         &modelpb.LabelValue{Global: true, Value: "elastic"},
			},
			NumericLabels: modelpb.NumericLabels{
				"user_id":     &modelpb.NumericLabelValue{Global: true, Value: 100},
				"cost_center": &modelpb.NumericLabelValue{Global: true, Value: 10},
			},
			Metricset: &modelpb.Metricset{
				Name:     "transaction",
				DocCount: 1,
				Interval: "1s",
			},
		},
		{
			Timestamp: timestamppb.New(time.Unix(0, 0).UTC()),
			Service: &modelpb.Service{
				Name: "svc",
			},
			Labels: modelpb.Labels{
				"department_name": &modelpb.LabelValue{Global: true, Value: "apm"},
				"organization":    &modelpb.LabelValue{Global: true, Value: "observability"},
				"company":         &modelpb.LabelValue{Global: true, Value: "elastic"},
			},
			NumericLabels: modelpb.NumericLabels{
				"user_id":     &modelpb.NumericLabelValue{Global: true, Value: 100},
				"cost_center": &modelpb.NumericLabelValue{Global: true, Value: 10},
			},
			Metricset: &modelpb.Metricset{
				Name:     "service_summary",
				Interval: "1s",
			},
		},
		{
			Timestamp: timestamppb.New(time.Unix(0, 0).UTC()),
			Event: &modelpb.Event{
				SuccessCount: &modelpb.SummaryMetric{
					Count: 1,
					Sum:   1,
				},
			},
			Transaction: &modelpb.Transaction{
				Type: "txtype",
				DurationSummary: &modelpb.SummaryMetric{
					Count: 1,
					Sum:   100351, // Estimate from histogram
				},
				DurationHistogram: &modelpb.Histogram{
					Values: []float64{100351},
					Counts: []uint64{1},
				},
			},
			Service: &modelpb.Service{
				Name: "svc",
			},
			Labels: modelpb.Labels{
				"department_name": &modelpb.LabelValue{Global: true, Value: "apm"},
				"organization":    &modelpb.LabelValue{Global: true, Value: "observability"},
				"company":         &modelpb.LabelValue{Global: true, Value: "elastic"},
			},
			NumericLabels: modelpb.NumericLabels{
				"user_id":     &modelpb.NumericLabelValue{Global: true, Value: 100},
				"cost_center": &modelpb.NumericLabelValue{Global: true, Value: 10},
			},
			Metricset: &modelpb.Metricset{
				Name:     "service_transaction",
				DocCount: 1,
				Interval: "1s",
			},
		},
	}
	assert.Empty(t, cmp.Diff(
		expected,
		events,
		cmpopts.IgnoreTypes(netip.Addr{}),
		cmpopts.SortSlices(func(a, b *modelpb.APMEvent) bool {
			return a.Metricset.Name < b.Metricset.Name
		}),
		protocmp.Transform(),
	))
}

func TestRunStopOrchestration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var firstHarvestDone atomic.Bool
	newAggregator := func() *Aggregator {
		agg, err := New(
			WithDataDir(t.TempDir()),
			WithProcessor(func(_ context.Context, _ CombinedMetricsKey, _ *aggregationpb.CombinedMetrics, _ time.Duration) error {
				firstHarvestDone.Swap(true)
				return nil
			}),
			WithAggregationIntervals([]time.Duration{time.Second}),
		)
		if err != nil {
			t.Fatal("failed to create test aggregator", err)
		}
		return agg
	}
	callAggregateBatch := func(agg *Aggregator) error {
		return agg.AggregateBatch(
			context.Background(),
			EncodeToCombinedMetricsKeyID(t, "ab01"),
			&modelpb.Batch{
				&modelpb.APMEvent{
					Event: &modelpb.Event{Duration: durationpb.New(time.Millisecond)},
					Transaction: &modelpb.Transaction{
						Name:                "T-1000",
						Type:                "type",
						RepresentativeCount: 1,
					},
				},
			},
		)
	}

	t.Run("run_before_close", func(t *testing.T) {
		agg := newAggregator()
		// Should aggregate even without running
		assert.NoError(t, callAggregateBatch(agg))
		go func() { agg.Run(ctx) }()
		assert.Eventually(t, func() bool {
			return firstHarvestDone.Load()
		}, 10*time.Second, 10*time.Millisecond, "failed while waiting for first harvest")
		assert.NoError(t, callAggregateBatch(agg))
		assert.NoError(t, agg.Close(ctx))
		assert.ErrorIs(t, callAggregateBatch(agg), ErrAggregatorClosed)
	})
	t.Run("close_before_run", func(t *testing.T) {
		agg := newAggregator()
		assert.NoError(t, agg.Close(ctx))
		assert.ErrorIs(t, callAggregateBatch(agg), ErrAggregatorClosed)
		assert.ErrorIs(t, agg.Run(ctx), ErrAggregatorClosed)
	})
	t.Run("multiple_run", func(t *testing.T) {
		agg := newAggregator()
		defer agg.Close(ctx)

		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error { return agg.Run(ctx) })
		g.Go(func() error { return agg.Run(ctx) })
		err := g.Wait()
		assert.Error(t, err)
		assert.EqualError(t, err, "aggregator is already running")
	})
	t.Run("multiple_close", func(t *testing.T) {
		agg := newAggregator()
		defer agg.Close(ctx)
		go func() { agg.Run(ctx) }()
		time.Sleep(time.Second)

		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error { return agg.Close(ctx) })
		g.Go(func() error { return agg.Close(ctx) })
		assert.NoError(t, g.Wait())
	})
}

func BenchmarkAggregateCombinedMetrics(b *testing.B) {
	gatherer, err := apmotel.NewGatherer()
	if err != nil {
		b.Fatal(err)
	}
	mp := metric.NewMeterProvider(metric.WithReader(gatherer))
	aggIvl := time.Minute
	agg, err := New(
		WithDataDir(b.TempDir()),
		WithLimits(Limits{
			MaxSpanGroups:                         1000,
			MaxSpanGroupsPerService:               100,
			MaxTransactionGroups:                  1000,
			MaxTransactionGroupsPerService:        100,
			MaxServiceTransactionGroups:           1000,
			MaxServiceTransactionGroupsPerService: 100,
			MaxServices:                           100,
			MaxServiceInstanceGroupsPerService:    100,
		}),
		WithProcessor(noOpProcessor()),
		WithMeter(mp.Meter("test")),
		WithLogger(zap.NewNop()),
	)
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		agg.Run(context.Background())
	}()
	b.Cleanup(func() {
		agg.Close(context.Background())
	})
	cmk := CombinedMetricsKey{
		Interval:       aggIvl,
		ProcessingTime: time.Now().Truncate(aggIvl),
		ID:             EncodeToCombinedMetricsKeyID(b, "ab01"),
	}
	cm := NewTestCombinedMetrics(WithEventsTotal(1)).
		AddServiceMetrics(serviceAggregationKey{
			Timestamp:   time.Now(),
			ServiceName: "test-svc",
		}).
		AddServiceInstanceMetrics(serviceInstanceAggregationKey{}).
		AddTransaction(transactionAggregationKey{
			TransactionName: "txntest",
			TransactionType: "txntype",
		}).
		AddServiceTransaction(serviceTransactionAggregationKey{
			TransactionType: "txntype",
		}).
		GetProto()
	b.Cleanup(func() { cm.ReturnToVTPool() })
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(func() { cancel() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := agg.AggregateCombinedMetrics(ctx, cmk, cm); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAggregateBatchSerial(b *testing.B) {
	b.ReportAllocs()
	agg := newTestAggregator(b)
	defer agg.Close(context.Background())
	batch := newTestBatchForBenchmark()
	cmID := EncodeToCombinedMetricsKeyID(b, "ab01")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := agg.AggregateBatch(context.Background(), cmID, batch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAggregateBatchParallel(b *testing.B) {
	b.ReportAllocs()
	agg := newTestAggregator(b)
	defer agg.Close(context.Background())
	batch := newTestBatchForBenchmark()
	cmID := EncodeToCombinedMetricsKeyID(b, "ab01")
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := agg.AggregateBatch(context.Background(), cmID, batch); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func newTestAggregator(tb testing.TB) *Aggregator {
	agg, err := New(
		WithDataDir(tb.TempDir()),
		WithLimits(Limits{
			MaxSpanGroups:                         1000,
			MaxSpanGroupsPerService:               100,
			MaxTransactionGroups:                  1000,
			MaxTransactionGroupsPerService:        100,
			MaxServiceTransactionGroups:           1000,
			MaxServiceTransactionGroupsPerService: 100,
			MaxServices:                           100,
			MaxServiceInstanceGroupsPerService:    100,
		}),
		WithProcessor(noOpProcessor()),
		WithAggregationIntervals([]time.Duration{time.Second, time.Minute, time.Hour}),
		WithLogger(zap.NewNop()),
	)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := agg.Close(context.Background()); err != nil {
			tb.Fatal(err)
		}
	})
	return agg
}

func newTestBatchForBenchmark() *modelpb.Batch {
	return &modelpb.Batch{
		&modelpb.APMEvent{
			Event: &modelpb.Event{Duration: durationpb.New(time.Millisecond)},
			Transaction: &modelpb.Transaction{
				Name:                "T-1000",
				Type:                "type",
				RepresentativeCount: 1,
			},
		},
	}
}

func noOpProcessor() Processor {
	return func(_ context.Context, _ CombinedMetricsKey, _ *aggregationpb.CombinedMetrics, _ time.Duration) error {
		return nil
	}
}

func combinedMetricsProcessor(out chan<- *aggregationpb.CombinedMetrics) Processor {
	return func(
		_ context.Context,
		_ CombinedMetricsKey,
		cm *aggregationpb.CombinedMetrics,
		_ time.Duration,
	) error {
		out <- cm.CloneVT()
		return nil
	}
}

func sliceProcessor(slice *[]*modelpb.APMEvent) Processor {
	return func(
		ctx context.Context,
		cmk CombinedMetricsKey,
		cm *aggregationpb.CombinedMetrics,
		aggregationIvl time.Duration,
	) error {
		batch, err := CombinedMetricsToBatch(cm, cmk.ProcessingTime, aggregationIvl)
		if err != nil {
			return err
		}
		if batch != nil {
			for _, e := range *batch {
				*slice = append(*slice, e)
			}
		}
		return nil
	}
}

type gatherMetricsCfg struct {
	ignoreMetricPrefix  string
	zeroHistogramValues bool
}

type gatherMetricsOpt func(gatherMetricsCfg) gatherMetricsCfg

// withIgnoreMetricPrefix ignores some metric prefixes from the gathered
// metrics.
func withIgnoreMetricPrefix(s string) gatherMetricsOpt {
	return func(cfg gatherMetricsCfg) gatherMetricsCfg {
		cfg.ignoreMetricPrefix = s
		return cfg
	}
}

// withZeroHistogramValues zeroes all histogram values if true. Useful
// for testing where histogram values are harder to estimate correctly.
func withZeroHistogramValues(b bool) gatherMetricsOpt {
	return func(cfg gatherMetricsCfg) gatherMetricsCfg {
		cfg.zeroHistogramValues = b
		return cfg
	}
}

func gatherMetrics(g apm.MetricsGatherer, opts ...gatherMetricsOpt) []apmmodel.Metrics {
	var cfg gatherMetricsCfg
	for _, opt := range opts {
		cfg = opt(cfg)
	}
	tracer := apmtest.NewRecordingTracer()
	defer tracer.Close()
	tracer.RegisterMetricsGatherer(g)
	tracer.SendMetrics(nil)
	metrics := tracer.Payloads().Metrics
	for i := range metrics {
		metrics[i].Timestamp = apmmodel.Time{}
	}

	for i, m := range metrics {
		for k, s := range m.Samples {
			// Remove internal metrics
			if strings.HasPrefix(k, "golang.") || strings.HasPrefix(k, "system.") {
				delete(m.Samples, k)
				continue
			}
			// Remove any metrics that has been explicitly ignored
			if cfg.ignoreMetricPrefix != "" && strings.HasPrefix(k, cfg.ignoreMetricPrefix) {
				delete(m.Samples, k)
				continue
			}
			// Zero out histogram values if required
			if s.Type == "histogram" && cfg.zeroHistogramValues {
				for j := range s.Values {
					s.Values[j] = 0
				}
			}
		}

		if len(m.Samples) == 0 {
			metrics[i] = metrics[len(metrics)-1]
			metrics = metrics[:len(metrics)-1]
		}
	}
	return metrics
}

func makeSpan(
	ts time.Time,
	serviceName, agentName, destinationServiceResource, targetType, targetName, outcome string,
	duration time.Duration,
	representativeCount float64,
	labels modelpb.Labels,
	numericLabels modelpb.NumericLabels,
) *modelpb.APMEvent {
	event := &modelpb.APMEvent{
		Timestamp: timestamppb.New(ts),
		Agent:     &modelpb.Agent{Name: agentName},
		Service:   &modelpb.Service{Name: serviceName},
		Event: &modelpb.Event{
			Outcome:  outcome,
			Duration: durationpb.New(duration),
		},
		Span: &modelpb.Span{
			Name:                serviceName + ":" + destinationServiceResource,
			Type:                "type",
			RepresentativeCount: representativeCount,
		},
		Labels:        labels,
		NumericLabels: numericLabels,
	}
	if destinationServiceResource != "" {
		event.Span.DestinationService = &modelpb.DestinationService{
			Resource: destinationServiceResource,
		}
	}
	if targetType != "" {
		event.Service.Target = &modelpb.ServiceTarget{
			Type: targetType,
			Name: targetName,
		}
	}
	return event
}
