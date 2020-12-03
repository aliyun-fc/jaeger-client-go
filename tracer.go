// Copyright (c) 2017-2018 Uber Technologies, Inc.
//
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

package jaeger

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"

	"github.com/uber/jaeger-client-go/internal/baggage"
	"github.com/uber/jaeger-client-go/internal/throttler"
	"github.com/uber/jaeger-client-go/log"
	"github.com/uber/jaeger-client-go/utils"
)

// Tracer implements opentracing.Tracer.
type Tracer struct {
	serviceName string
	hostIPv4    uint32 // this is for zipkin endpoint conversion

	sampler  Sampler
	reporter Reporter
	metrics  Metrics
	logger   log.Logger

	timeNow      func() time.Time
	randomNumber func() uint64

	options struct {
		gen128Bit                   bool // whether to generate 128bit trace IDs
		zipkinSharedRPCSpan         bool
		highTraceIDGenerator        func() uint64 // custom high trace ID generator
		maxTagValueLength           int
		noDebugFlagOnForcedSampling bool
		// more options to come
	}
	// allocator of Span objects
	spanAllocator SpanAllocator

	injectors  map[interface{}]Injector
	extractors map[interface{}]Extractor

	observer compositeObserver

	tags    []Tag
	process Process

	baggageRestrictionManager baggage.RestrictionManager
	baggageSetter             *baggageSetter

	debugThrottler throttler.Throttler
}

// NewTracer creates Tracer implementation that reports tracing to Jaeger.
// The returned io.Closer can be used in shutdown hooks to ensure that the internal
// queue of the Reporter is drained and all buffered spans are submitted to collectors.
func NewTracer(
	serviceName string,
	sampler Sampler,
	reporter Reporter,
	options ...TracerOption,
) (opentracing.Tracer, io.Closer) {
	t := &Tracer{
		serviceName:   serviceName,
		sampler:       sampler,
		reporter:      reporter,
		injectors:     make(map[interface{}]Injector),
		extractors:    make(map[interface{}]Extractor),
		metrics:       *NewNullMetrics(),
		spanAllocator: simpleSpanAllocator{},
	}

	for _, option := range options {
		option(t)
	}

	// register default injectors/extractors unless they are already provided via options
	textPropagator := NewTextMapPropagator(getDefaultHeadersConfig(), t.metrics)
	t.addCodec(opentracing.TextMap, textPropagator, textPropagator)

	httpHeaderPropagator := NewHTTPHeaderPropagator(getDefaultHeadersConfig(), t.metrics)
	t.addCodec(opentracing.HTTPHeaders, httpHeaderPropagator, httpHeaderPropagator)

	binaryPropagator := NewBinaryPropagator(t)
	t.addCodec(opentracing.Binary, binaryPropagator, binaryPropagator)

	// TODO remove after TChannel supports OpenTracing
	interopPropagator := &jaegerTraceContextPropagator{tracer: t}
	t.addCodec(SpanContextFormat, interopPropagator, interopPropagator)

	zipkinPropagator := &zipkinPropagator{tracer: t}
	t.addCodec(ZipkinSpanFormat, zipkinPropagator, zipkinPropagator)

	if t.baggageRestrictionManager != nil {
		t.baggageSetter = newBaggageSetter(t.baggageRestrictionManager, &t.metrics)
	} else {
		t.baggageSetter = newBaggageSetter(baggage.NewDefaultRestrictionManager(0), &t.metrics)
	}
	if t.debugThrottler == nil {
		t.debugThrottler = throttler.DefaultThrottler{}
	}

	if t.randomNumber == nil {
		seedGenerator := utils.NewRand(time.Now().UnixNano())
		pool := sync.Pool{
			New: func() interface{} {
				return rand.NewSource(seedGenerator.Int63())
			},
		}

		t.randomNumber = func() uint64 {
			generator := pool.Get().(rand.Source)
			number := uint64(generator.Int63())
			pool.Put(generator)
			return number
		}
	}
	if t.timeNow == nil {
		t.timeNow = time.Now
	}
	if t.logger == nil {
		t.logger = log.NullLogger
	}
	// Set tracer-level tags
	t.tags = append(t.tags, Tag{key: JaegerClientVersionTagKey, value: JaegerClientVersion})

	if _, ok := t.getTag(TracerHostnameTagKey); !ok {
		if hostname, err := os.Hostname(); err == nil {
			t.tags = append(t.tags, Tag{key: TracerHostnameTagKey, value: hostname})
		}
	}

	if ipval, ok := t.getTag(TracerIPTagKey); ok {
		ipv4, err := utils.ParseIPToUint32(ipval.(string))
		if err != nil {
			t.hostIPv4 = 0
			t.logger.Error("Unable to convert the externally provided ip to uint32: " + err.Error())
		} else {
			t.hostIPv4 = ipv4
		}
	} else if ip, err := utils.HostIP(); err == nil {
		t.tags = append(t.tags, Tag{key: TracerIPTagKey, value: ip.String()})
		t.hostIPv4 = utils.PackIPAsUint32(ip)
	} else {
		t.logger.Error("Unable to determine this host's IP address: " + err.Error())
	}

	if t.options.gen128Bit {
		if t.options.highTraceIDGenerator == nil {
			t.options.highTraceIDGenerator = t.randomNumber
		}
	} else if t.options.highTraceIDGenerator != nil {
		t.logger.Error("Overriding high trace ID generator but not generating " +
			"128 bit trace IDs, consider enabling the \"Gen128Bit\" option")
	}
	if t.options.maxTagValueLength == 0 {
		t.options.maxTagValueLength = DefaultMaxTagValueLength
	}
	t.process = Process{
		Service: serviceName,
		UUID:    strconv.FormatUint(t.randomNumber(), 16),
		Tags:    t.tags,
	}
	if throttler, ok := t.debugThrottler.(ProcessSetter); ok {
		throttler.SetProcess(t.process)
	}

	return t, t
}

// addCodec adds registers injector and extractor for given propagation format if not already defined.
func (t *Tracer) addCodec(format interface{}, injector Injector, extractor Extractor) {
	if _, ok := t.injectors[format]; !ok {
		t.injectors[format] = injector
	}
	if _, ok := t.extractors[format]; !ok {
		t.extractors[format] = extractor
	}
}

// StartSpan implements StartSpan() method of opentracing.Tracer.
func (t *Tracer) StartSpan(
	operationName string,
	options ...opentracing.StartSpanOption,
) opentracing.Span {
	sso := opentracing.StartSpanOptions{}
	for _, o := range options {
		o.Apply(&sso)
	}
	return t.startSpanWithOptions(operationName, sso)
}

func (t *Tracer) startSpanWithOptions(
	operationName string,
	options opentracing.StartSpanOptions,
) opentracing.Span {
	if options.StartTime.IsZero() {
		options.StartTime = t.timeNow()
	}

	// Predicate whether the given span context is a valid reference
	// which may be used as parent / debug ID / baggage items source
	isValidReference := func(ctx SpanContext) bool {
		return ctx.IsValid() || ctx.isDebugIDContainerOnly() || len(ctx.baggage) != 0
	}

	var references []Reference
	var parent SpanContext
	var hasParent bool // need this because `parent` is a value, not reference
	var ctx SpanContext
	var isSelfRef bool
	for _, ref := range options.References {
		ctxRef, ok := ref.ReferencedContext.(SpanContext)
		if !ok {
			t.logger.Error(fmt.Sprintf(
				"Reference contains invalid type of SpanReference: %s",
				reflect.ValueOf(ref.ReferencedContext)))
			continue
		}
		if !isValidReference(ctxRef) {
			continue
		}

		if ref.Type == selfRefType {
			isSelfRef = true
			ctx = ctxRef
			continue
		}

		references = append(references, Reference{Type: ref.Type, Context: ctxRef})

		if !hasParent {
			parent = ctxRef
			hasParent = ref.Type == opentracing.ChildOfRef
		}
	}
	if !hasParent && isValidReference(parent) {
		// If ChildOfRef wasn't found but a FollowFromRef exists, use the context from
		// the FollowFromRef as the parent
		hasParent = true
	}

	rpcServer := false
	if v, ok := options.Tags[ext.SpanKindRPCServer.Key]; ok {
		rpcServer = (v == ext.SpanKindRPCServerEnum || v == string(ext.SpanKindRPCServerEnum))
	}

	var samplerTags []Tag
	newTrace := false
	if !isSelfRef {
		if !hasParent || !parent.IsValid() {
			newTrace = true
			ctx.traceID.Low = t.randomID()
			if t.options.gen128Bit {
				ctx.traceID.High = t.options.highTraceIDGenerator()
			}
			ctx.spanID = SpanID(ctx.traceID.Low)
			ctx.parentID = 0
			ctx.flags = byte(0)
			if hasParent && parent.isDebugIDContainerOnly() && t.isDebugAllowed(operationName) {
				ctx.flags |= (flagSampled | flagDebug)
				samplerTags = []Tag{{key: JaegerDebugHeader, value: parent.debugID}}
			} else if sampled, tags := t.sampler.IsSampled(ctx.traceID, operationName); sampled {
				ctx.flags |= flagSampled
				samplerTags = tags
			}
		} else {
			ctx.traceID = parent.traceID
			if rpcServer && t.options.zipkinSharedRPCSpan {
				// Support Zipkin's one-span-per-RPC model
				ctx.spanID = parent.spanID
				ctx.parentID = parent.parentID
			} else {
				ctx.spanID = SpanID(t.randomID())
				ctx.parentID = parent.spanID
			}
			ctx.flags = parent.flags
		}
		if hasParent {
			// copy baggage items
			if l := len(parent.baggage); l > 0 {
				ctx.baggage = make(map[string]string, len(parent.baggage))
				for k, v := range parent.baggage {
					ctx.baggage[k] = v
				}
			}
		}
	}

	sp := t.newSpan()
	sp.context = ctx
	sp.observer = t.observer.OnStartSpan(sp, operationName, options)
	return t.startSpanInternal(
		sp,
		operationName,
		options.StartTime,
		samplerTags,
		options.Tags,
		newTrace,
		rpcServer,
		references,
	)
}

// Inject implements Inject() method of opentracing.Tracer
func (t *Tracer) Inject(ctx opentracing.SpanContext, format interface{}, carrier interface{}) error {
	c, ok := ctx.(SpanContext)
	if !ok {
		return opentracing.ErrInvalidSpanContext
	}
	if injector, ok := t.injectors[format]; ok {
		return injector.Inject(c, carrier)
	}
	return opentracing.ErrUnsupportedFormat
}

// Extract implements Extract() method of opentracing.Tracer
func (t *Tracer) Extract(
	format interface{},
	carrier interface{},
) (opentracing.SpanContext, error) {
	if extractor, ok := t.extractors[format]; ok {
		spanCtx, err := extractor.Extract(carrier)
		if err != nil {
			return nil, err // ensure returned spanCtx is nil
		}
		return spanCtx, nil
	}
	return nil, opentracing.ErrUnsupportedFormat
}

// Close releases all resources used by the Tracer and flushes any remaining buffered spans.
func (t *Tracer) Close() error {
	t.sampler.Close()
	if mgr, ok := t.baggageRestrictionManager.(io.Closer); ok {
		mgr.Close()
	}
	if throttler, ok := t.debugThrottler.(io.Closer); ok {
		throttler.Close()
	}
	return nil
}

// Tags returns a slice of tracer-level tags.
func (t *Tracer) Tags() []opentracing.Tag {
	tags := make([]opentracing.Tag, len(t.tags))
	for i, tag := range t.tags {
		tags[i] = opentracing.Tag{Key: tag.key, Value: tag.value}
	}
	return tags
}

// getTag returns the value of specific tag, if not exists, return nil.
func (t *Tracer) getTag(key string) (interface{}, bool) {
	for _, tag := range t.tags {
		if tag.key == key {
			return tag.value, true
		}
	}
	return nil, false
}

// newSpan returns an instance of a clean Span object.
// If options.PoolSpans is true, the spans are retrieved from an object pool.
func (t *Tracer) newSpan() *Span {
	return t.spanAllocator.Get()
}

func (t *Tracer) startSpanInternal(
	sp *Span,
	operationName string,
	startTime time.Time,
	internalTags []Tag,
	tags opentracing.Tags,
	newTrace bool,
	rpcServer bool,
	references []Reference,
) *Span {
	sp.tracer = t
	sp.operationName = operationName
	sp.startTime = startTime
	sp.duration = 0
	sp.references = references
	sp.firstInProcess = rpcServer || sp.context.parentID == 0
	if len(tags) > 0 || len(internalTags) > 0 {
		sp.tags = make([]Tag, len(internalTags), len(tags)+len(internalTags))
		copy(sp.tags, internalTags)
		for k, v := range tags {
			sp.observer.OnSetTag(k, v)
			if k == string(ext.SamplingPriority) && !setSamplingPriority(sp, v) {
				continue
			}
			sp.setTagNoLocking(k, v)
		}
	}
	// emit metrics
	if sp.context.IsSampled() {
		t.metrics.SpansStartedSampled.Inc(1)
		if newTrace {
			// We cannot simply check for parentID==0 because in Zipkin model the
			// server-side RPC span has the exact same trace/span/parent IDs as the
			// calling client-side span, but obviously the server side span is
			// no longer a root span of the trace.
			t.metrics.TracesStartedSampled.Inc(1)
		} else if sp.firstInProcess {
			t.metrics.TracesJoinedSampled.Inc(1)
		}
	} else {
		t.metrics.SpansStartedNotSampled.Inc(1)
		if newTrace {
			t.metrics.TracesStartedNotSampled.Inc(1)
		} else if sp.firstInProcess {
			t.metrics.TracesJoinedNotSampled.Inc(1)
		}
	}
	return sp
}

func (t *Tracer) reportSpan(sp *Span) {
	t.metrics.SpansFinished.Inc(1)

	// Note: if the reporter is processing Span asynchronously need to Retain() it
	// otherwise, in the racing condition will be rewritten span data before it will be sent
	// * To remove object use method span.Release()
	if sp.context.IsSampled() {
		t.reporter.Report(sp)
	}

	sp.Release()
}

// randomID generates a random trace/span ID, using tracer.random() generator.
// It never returns 0.
func (t *Tracer) randomID() uint64 {
	val := t.randomNumber()
	for val == 0 {
		val = t.randomNumber()
	}
	return val
}

// (NB) span must hold the lock before making this call
func (t *Tracer) setBaggage(sp *Span, key, value string) {
	t.baggageSetter.setBaggage(sp, key, value)
}

// (NB) span must hold the lock before making this call
func (t *Tracer) isDebugAllowed(operation string) bool {
	return t.debugThrottler.IsAllowed(operation)
}

// SelfRef creates an opentracing compliant SpanReference from a jaeger
// SpanContext. This is a factory function in order to encapsulate jaeger specific
// types.
func SelfRef(ctx SpanContext) opentracing.SpanReference {
	return opentracing.SpanReference{
		Type:              selfRefType,
		ReferencedContext: ctx,
	}
}
