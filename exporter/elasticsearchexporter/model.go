// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package elasticsearchexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter"

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pprofile"
	"go.opentelemetry.io/collector/pdata/ptrace"
	semconv "go.opentelemetry.io/otel/semconv/v1.22.0"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter/internal/datapoints"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter/internal/elasticsearch"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter/internal/objmodel"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter/internal/serializer"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter/internal/serializer/otelserializer"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/traceutil"
)

// resourceAttrsConversionMap contains conversions for resource-level attributes
// from their Semantic Conventions (SemConv) names to equivalent Elastic Common
// Schema (ECS) names.
// If the ECS field name is specified as an empty string (""), the converter will
// neither convert the SemConv key to the equivalent ECS name nor pass-through the
// SemConv key as-is to become the ECS name.
var resourceAttrsConversionMap = map[string]string{
	string(semconv.ServiceInstanceIDKey):      "service.node.name",
	string(semconv.DeploymentEnvironmentKey):  "service.environment",
	string(semconv.TelemetrySDKNameKey):       "",
	string(semconv.TelemetrySDKLanguageKey):   "",
	string(semconv.TelemetrySDKVersionKey):    "",
	string(semconv.TelemetryDistroNameKey):    "",
	string(semconv.TelemetryDistroVersionKey): "",
	string(semconv.CloudPlatformKey):          "cloud.service.name",
	string(semconv.ContainerImageTagsKey):     "container.image.tag",
	string(semconv.HostNameKey):               "host.hostname",
	string(semconv.HostArchKey):               "host.architecture",
	string(semconv.ProcessExecutablePathKey):  "process.executable",
	string(semconv.ProcessRuntimeNameKey):     "service.runtime.name",
	string(semconv.ProcessRuntimeVersionKey):  "service.runtime.version",
	string(semconv.OSNameKey):                 "host.os.name",
	string(semconv.OSTypeKey):                 "host.os.platform",
	string(semconv.OSDescriptionKey):          "host.os.full",
	string(semconv.OSVersionKey):              "host.os.version",
	string(semconv.K8SDeploymentNameKey):      "kubernetes.deployment.name",
	string(semconv.K8SNamespaceNameKey):       "kubernetes.namespace",
	string(semconv.K8SNodeNameKey):            "kubernetes.node.name",
	string(semconv.K8SPodNameKey):             "kubernetes.pod.name",
	string(semconv.K8SPodUIDKey):              "kubernetes.pod.uid",
	string(semconv.K8SJobNameKey):             "kubernetes.job.name",
	string(semconv.K8SCronJobNameKey):         "kubernetes.cronjob.name",
	string(semconv.K8SStatefulSetNameKey):     "kubernetes.statefulset.name",
	string(semconv.K8SReplicaSetNameKey):      "kubernetes.replicaset.name",
	string(semconv.K8SDaemonSetNameKey):       "kubernetes.daemonset.name",
	string(semconv.K8SContainerNameKey):       "kubernetes.container.name",
	string(semconv.K8SClusterNameKey):         "orchestrator.cluster.name",
}

// resourceAttrsToPreserve contains conventions that should be preserved in ECS mode.
// This can happen when an attribute needs to be mapped to an ECS equivalent but
// at the same time be preserved to its original form.
var resourceAttrsToPreserve = map[string]bool{
	string(semconv.HostNameKey): true,
}

var ErrInvalidTypeForBodyMapMode = errors.New("invalid log record body type for 'bodymap' mapping mode")

// documentEncoder is an interface for encoding signals to Elasticsearch documents.
type documentEncoder interface {
	encodeLog(encodingContext, plog.LogRecord, elasticsearch.Index, *bytes.Buffer) error
	encodeSpan(encodingContext, ptrace.Span, elasticsearch.Index, *bytes.Buffer) error
	encodeSpanEvent(encodingContext, ptrace.Span, ptrace.SpanEvent, elasticsearch.Index, *bytes.Buffer) error
	encodeMetrics(_ encodingContext, _ []datapoints.DataPoint, validationErrors *[]error, _ elasticsearch.Index, _ *bytes.Buffer) (map[string]string, error)
	encodeProfile(_ encodingContext, _ pprofile.ProfilesDictionary, _ pprofile.Profile, _ func(*bytes.Buffer, string, string) error) error
}

type encodingContext struct {
	resource          pcommon.Resource
	resourceSchemaURL string
	scope             pcommon.InstrumentationScope
	scopeSchemaURL    string
}

func newEncoder(mode MappingMode) (documentEncoder, error) {
	switch mode {
	case MappingNone:
		return legacyModeEncoder{
			metricsUnsupportedEncoder:  metricsUnsupportedEncoder{mode: mode},
			profilesUnsupportedEncoder: profilesUnsupportedEncoder{mode: mode},
			nonOTelSpanEncoder: nonOTelSpanEncoder{
				attributesPrefix: "Attributes",
				eventsPrefix:     "Events",
			},
			attributesPrefix: "Attributes",
		}, nil
	case MappingRaw:
		return legacyModeEncoder{
			metricsUnsupportedEncoder:  metricsUnsupportedEncoder{mode: mode},
			profilesUnsupportedEncoder: profilesUnsupportedEncoder{mode: mode},
			nonOTelSpanEncoder: nonOTelSpanEncoder{
				attributesPrefix: "",
				eventsPrefix:     "",
			},
			attributesPrefix: "",
		}, nil
	case MappingECS:
		return ecsModeEncoder{
			profilesUnsupportedEncoder: profilesUnsupportedEncoder{mode: mode},
		}, nil
	case MappingBodyMap:
		return bodymapModeEncoder{
			metricsUnsupportedEncoder:  metricsUnsupportedEncoder{mode: mode},
			profilesUnsupportedEncoder: profilesUnsupportedEncoder{mode: mode},
		}, nil
	case MappingOTel:
		ser, err := otelserializer.New()
		if err != nil {
			return nil, err
		}
		return otelModeEncoder{serializer: ser}, nil
	}
	return nil, fmt.Errorf("unknown mapping mode %q (%d)", mode, int(mode))
}

type legacyModeEncoder struct {
	nonOTelSpanEncoder
	nopSpanEventEncoder
	metricsUnsupportedEncoder
	profilesUnsupportedEncoder
	attributesPrefix string
}

type ecsModeEncoder struct {
	ecsDataPointsEncoder
	nopSpanEventEncoder
	profilesUnsupportedEncoder
}

type bodymapModeEncoder struct {
	metricsUnsupportedEncoder
	profilesUnsupportedEncoder
}

type otelModeEncoder struct {
	serializer *otelserializer.Serializer
}

const (
	traceIDField   = "traceID"
	spanIDField    = "spanID"
	attributeField = "attribute"
)

func (e legacyModeEncoder) encodeLog(ec encodingContext, record plog.LogRecord, idx elasticsearch.Index, buf *bytes.Buffer) error {
	var document objmodel.Document

	docTimeStamp := record.Timestamp()
	if docTimeStamp.AsTime().UnixNano() == 0 {
		docTimeStamp = record.ObservedTimestamp()
	}
	// We use @timestamp in order to ensure that we can index if the default data stream logs template is used.
	document.AddTimestamp("@timestamp", docTimeStamp)
	document.AddTraceID("TraceId", record.TraceID())
	document.AddSpanID("SpanId", record.SpanID())
	document.AddInt("TraceFlags", int64(record.Flags()))
	document.AddString("SeverityText", record.SeverityText())
	document.AddInt("SeverityNumber", int64(record.SeverityNumber()))
	document.AddAttribute("Body", record.Body())
	document.AddAttributes("Resource", ec.resource.Attributes())
	document.AddAttributes("Scope", scopeToAttributes(ec.scope))
	encodeAttributes(e.attributesPrefix, &document, record.Attributes(), idx)

	return document.Serialize(buf, false)
}

func (ecsModeEncoder) encodeLog(
	ec encodingContext,
	record plog.LogRecord,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	var document objmodel.Document

	// First, try to map resource-level attributes to ECS fields.
	encodeAttributesECSMode(&document, ec.resource.Attributes(), resourceAttrsConversionMap, resourceAttrsToPreserve)

	// Then, try to map scope-level attributes to ECS fields.
	scopeAttrsConversionMap := map[string]string{
		// None at the moment
	}
	encodeAttributesECSMode(&document, ec.scope.Attributes(), scopeAttrsConversionMap, resourceAttrsToPreserve)

	// Finally, try to map record-level attributes to ECS fields.
	recordAttrsConversionMap := map[string]string{
		"event.name":                           "event.action",
		string(semconv.ExceptionMessageKey):    "error.message",
		string(semconv.ExceptionStacktraceKey): "error.stacktrace",
		string(semconv.ExceptionTypeKey):       "error.type",
		string(semconv.ExceptionEscapedKey):    "event.error.exception.handled",
	}
	encodeAttributesECSMode(&document, record.Attributes(), recordAttrsConversionMap, resourceAttrsToPreserve)
	addDataStreamAttributes(&document, "", idx)

	// Handle special cases.
	encodeLogAgentNameECSMode(&document, ec.resource)
	encodeLogAgentVersionECSMode(&document, ec.resource)
	encodeHostOsTypeECSMode(&document, ec.resource)
	encodeLogTimestampECSMode(&document, record)
	document.AddTraceID("trace.id", record.TraceID())
	document.AddSpanID("span.id", record.SpanID())
	if n := record.SeverityNumber(); n != plog.SeverityNumberUnspecified {
		document.AddInt("event.severity", int64(record.SeverityNumber()))
	}

	document.AddString("log.level", record.SeverityText())

	if record.Body().Type() == pcommon.ValueTypeStr {
		document.AddAttribute("message", record.Body())
	}

	return document.Serialize(buf, true)
}

func (ecsModeEncoder) encodeSpan(
	ec encodingContext,
	span ptrace.Span,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	var document objmodel.Document

	// First, try to map resource-level attributes to ECS fields.
	encodeAttributesECSMode(&document, ec.resource.Attributes(), resourceAttrsConversionMap, resourceAttrsToPreserve)

	// Then, try to map scope-level attributes to ECS fields.
	scopeAttrsConversionMap := map[string]string{
		// None at the moment
	}
	encodeAttributesECSMode(&document, ec.scope.Attributes(), scopeAttrsConversionMap, resourceAttrsToPreserve)

	// Finally, try to map record-level attributes to ECS fields.
	spanAttrsConversionMap := map[string]string{
		// None at the moment
	}

	// Handle special cases.
	encodeAttributesECSMode(&document, span.Attributes(), spanAttrsConversionMap, resourceAttrsToPreserve)
	encodeHostOsTypeECSMode(&document, ec.resource)
	addDataStreamAttributes(&document, "", idx)

	document.AddTimestamp("@timestamp", span.StartTimestamp())
	document.AddTraceID("trace.id", span.TraceID())
	document.AddSpanID("span.id", span.SpanID())
	document.AddString("span.name", span.Name())
	document.AddSpanID("parent.id", span.ParentSpanID())
	if span.Status().Code() == ptrace.StatusCodeOk {
		document.AddString("event.outcome", "success")
	} else if span.Status().Code() == ptrace.StatusCodeError {
		document.AddString("event.outcome", "failure")
	}
	document.AddLinks("span.links", span.Links())

	return document.Serialize(buf, true)
}

func (e otelModeEncoder) encodeLog(
	ec encodingContext,
	record plog.LogRecord,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	return e.serializer.SerializeLog(
		ec.resource, ec.resourceSchemaURL,
		ec.scope, ec.scopeSchemaURL,
		record, idx, buf,
	)
}

func (e otelModeEncoder) encodeSpan(
	ec encodingContext,
	span ptrace.Span,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	return e.serializer.SerializeSpan(
		ec.resource, ec.resourceSchemaURL,
		ec.scope, ec.scopeSchemaURL,
		span, idx, buf,
	)
}

func (e otelModeEncoder) encodeSpanEvent(
	ec encodingContext,
	span ptrace.Span,
	spanEvent ptrace.SpanEvent,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	e.serializer.SerializeSpanEvent(
		ec.resource, ec.resourceSchemaURL,
		ec.scope, ec.scopeSchemaURL,
		span, spanEvent, idx, buf,
	)
	return nil
}

func (e otelModeEncoder) encodeMetrics(
	ec encodingContext,
	dataPoints []datapoints.DataPoint,
	validationErrors *[]error,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) (map[string]string, error) {
	return e.serializer.SerializeMetrics(
		ec.resource, ec.resourceSchemaURL,
		ec.scope, ec.scopeSchemaURL,
		dataPoints, validationErrors, idx, buf,
	)
}

func (e otelModeEncoder) encodeProfile(
	ec encodingContext,
	dic pprofile.ProfilesDictionary,
	profile pprofile.Profile,
	pushData func(*bytes.Buffer, string, string) error,
) error {
	return e.serializer.SerializeProfile(dic, ec.resource, ec.scope, profile, pushData)
}

func (bodymapModeEncoder) encodeLog(
	_ encodingContext,
	record plog.LogRecord,
	_ elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	body := record.Body()
	if body.Type() != pcommon.ValueTypeMap {
		return fmt.Errorf("%w: %q", ErrInvalidTypeForBodyMapMode, body.Type())
	}
	serializer.Map(body.Map(), buf)
	return nil
}

func (bodymapModeEncoder) encodeSpan(encodingContext, ptrace.Span, elasticsearch.Index, *bytes.Buffer) error {
	return errors.New("bodymap mode does not support encoding spans")
}

func (bodymapModeEncoder) encodeSpanEvent(encodingContext, ptrace.Span, ptrace.SpanEvent, elasticsearch.Index, *bytes.Buffer) error {
	return errors.New("bodymap mode does not support encoding span events")
}

type metricsUnsupportedEncoder struct {
	mode MappingMode
}

//nolint:unparam // result 0 is expected to always be nil
func (e metricsUnsupportedEncoder) encodeMetrics(
	_ encodingContext,
	_ []datapoints.DataPoint,
	_ *[]error,
	_ elasticsearch.Index,
	_ *bytes.Buffer,
) (map[string]string, error) {
	return nil, fmt.Errorf("mapping mode %q (%d) does not support metrics", e.mode, int(e.mode))
}

type profilesUnsupportedEncoder struct {
	mode MappingMode
}

func (e profilesUnsupportedEncoder) encodeProfile(
	_ encodingContext, _ pprofile.ProfilesDictionary, _ pprofile.Profile, _ func(*bytes.Buffer, string, string) error,
) error {
	return fmt.Errorf("mapping mode %q (%d) does not support profiles", e.mode, int(e.mode))
}

type nonOTelSpanEncoder struct {
	attributesPrefix string
	eventsPrefix     string
	dedot            bool
}

func (e nonOTelSpanEncoder) encodeSpan(
	ec encodingContext,
	span ptrace.Span,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) error {
	var document objmodel.Document
	document.AddTimestamp("@timestamp", span.StartTimestamp()) // We use @timestamp in order to ensure that we can index if the default data stream logs template is used.
	document.AddTimestamp("EndTimestamp", span.EndTimestamp())
	document.AddTraceID("TraceId", span.TraceID())
	document.AddSpanID("SpanId", span.SpanID())
	document.AddSpanID("ParentSpanId", span.ParentSpanID())
	document.AddString("Name", span.Name())
	document.AddString("Kind", traceutil.SpanKindStr(span.Kind()))
	document.AddInt("TraceStatus", int64(span.Status().Code()))
	document.AddString("TraceStatusDescription", span.Status().Message())
	document.AddString("Link", spanLinksToString(span.Links()))
	document.AddAttributes("Resource", ec.resource.Attributes())
	document.AddInt("Duration", durationAsMicroseconds(span.StartTimestamp().AsTime(), span.EndTimestamp().AsTime())) // unit is microseconds
	document.AddAttributes("Scope", scopeToAttributes(ec.scope))
	encodeAttributes(e.attributesPrefix, &document, span.Attributes(), idx)
	document.AddEvents(e.eventsPrefix, span.Events())
	return document.Serialize(buf, e.dedot)
}

type ecsDataPointsEncoder struct{}

func (ecsDataPointsEncoder) encodeMetrics(
	ec encodingContext,
	dataPoints []datapoints.DataPoint,
	validationErrors *[]error,
	idx elasticsearch.Index,
	buf *bytes.Buffer,
) (map[string]string, error) {
	dp0 := dataPoints[0]
	var document objmodel.Document
	encodeAttributesECSMode(&document, ec.resource.Attributes(), resourceAttrsConversionMap, resourceAttrsToPreserve)
	document.AddTimestamp("@timestamp", dp0.Timestamp())
	document.AddAttributes("", dp0.Attributes())
	addDataStreamAttributes(&document, "", idx)

	for _, dp := range dataPoints {
		value, err := dp.Value()
		if err != nil {
			*validationErrors = append(*validationErrors, err)
			continue
		}
		document.AddAttribute(dp.Metric().Name(), value)
	}
	err := document.Serialize(buf, true)

	return document.DynamicTemplates(), err
}

func addDataStreamAttributes(document *objmodel.Document, key string, idx elasticsearch.Index) {
	if idx.IsDataStream() {
		document.AddString(key+"data_stream.type", idx.Type)
		document.AddString(key+"data_stream.dataset", idx.Dataset)
		document.AddString(key+"data_stream.namespace", idx.Namespace)
	}
}

// nopSpanEventEncoder is embedded in all non-OTel encoders,
// since only OTel mapping mode currently encodes span events
// as separate documents. In all others they are stored within
// the span document.
type nopSpanEventEncoder struct{}

func (nopSpanEventEncoder) encodeSpanEvent(encodingContext, ptrace.Span, ptrace.SpanEvent, elasticsearch.Index, *bytes.Buffer) error {
	return nil
}

func encodeAttributes(prefix string, document *objmodel.Document, attributes pcommon.Map, idx elasticsearch.Index) {
	document.AddAttributes(prefix, attributes)
	addDataStreamAttributes(document, prefix, idx)
}

func spanLinksToString(spanLinkSlice ptrace.SpanLinkSlice) string {
	linkArray := make([]map[string]any, 0, spanLinkSlice.Len())
	for _, spanLink := range spanLinkSlice.All() {
		link := map[string]any{}
		link[spanIDField] = traceutil.SpanIDToHexOrEmptyString(spanLink.SpanID())
		link[traceIDField] = traceutil.TraceIDToHexOrEmptyString(spanLink.TraceID())
		link[attributeField] = spanLink.Attributes().AsRaw()
		linkArray = append(linkArray, link)
	}
	linkArrayBytes, _ := json.Marshal(&linkArray)
	return string(linkArrayBytes)
}

// durationAsMicroseconds calculate span duration through end - start nanoseconds and converts time.Time to microseconds,
// which is the format the Duration field is stored in the Span.
func durationAsMicroseconds(start, end time.Time) int64 {
	return (end.UnixNano() - start.UnixNano()) / 1000
}

func scopeToAttributes(scope pcommon.InstrumentationScope) pcommon.Map {
	attrs := pcommon.NewMap()

	scope.Attributes().CopyTo(attrs)

	attrs.PutStr("name", scope.Name())
	attrs.PutStr("version", scope.Version())

	return attrs
}

func encodeAttributesECSMode(document *objmodel.Document, attrs pcommon.Map, conversionMap map[string]string, preserveMap map[string]bool) {
	if len(conversionMap) == 0 {
		// No conversions to be done; add all attributes at top level of
		// document.
		document.AddAttributes("", attrs)
		return
	}

	for k, v := range attrs.All() {
		// If ECS key is found for current k in conversion map, use it.
		if ecsKey, exists := conversionMap[k]; exists {
			if ecsKey == "" {
				// Skip the conversion for this k.
				continue
			}

			document.AddAttribute(ecsKey, v)
			if preserve := preserveMap[k]; preserve {
				document.AddAttribute(k, v)
			}
			continue
		}

		// Otherwise, add key at top level with attribute name as-is.
		document.AddAttribute(k, v)
	}
}

func encodeLogAgentNameECSMode(document *objmodel.Document, resource pcommon.Resource) {
	// Parse out telemetry SDK name, language, and distro name from resource
	// attributes, setting defaults as needed.
	telemetrySdkName := "otlp"
	var telemetrySdkLanguage, telemetryDistroName string

	attrs := resource.Attributes()
	if v, exists := attrs.Get(string(semconv.TelemetrySDKNameKey)); exists {
		telemetrySdkName = v.Str()
	}
	if v, exists := attrs.Get(string(semconv.TelemetrySDKLanguageKey)); exists {
		telemetrySdkLanguage = v.Str()
	}
	if v, exists := attrs.Get(string(semconv.TelemetryDistroNameKey)); exists {
		telemetryDistroName = v.Str()
		if telemetrySdkLanguage == "" {
			telemetrySdkLanguage = "unknown"
		}
	}

	// Construct agent name from telemetry SDK name, language, and distro name.
	agentName := telemetrySdkName
	if telemetryDistroName != "" {
		agentName = fmt.Sprintf("%s/%s/%s", agentName, telemetrySdkLanguage, telemetryDistroName)
	} else if telemetrySdkLanguage != "" {
		agentName = fmt.Sprintf("%s/%s", agentName, telemetrySdkLanguage)
	}

	// Set agent name in document.
	document.AddString("agent.name", agentName)
}

func encodeLogAgentVersionECSMode(document *objmodel.Document, resource pcommon.Resource) {
	attrs := resource.Attributes()

	if telemetryDistroVersion, exists := attrs.Get(string(semconv.TelemetryDistroVersionKey)); exists {
		document.AddString("agent.version", telemetryDistroVersion.Str())
		return
	}

	if telemetrySdkVersion, exists := attrs.Get(string(semconv.TelemetrySDKVersionKey)); exists {
		document.AddString("agent.version", telemetrySdkVersion.Str())
		return
	}
}

func encodeHostOsTypeECSMode(document *objmodel.Document, resource pcommon.Resource) {
	// https://www.elastic.co/guide/en/ecs/current/ecs-os.html#field-os-type:
	//
	// "One of these following values should be used (lowercase): linux, macos, unix, windows.
	// If the OS you’re dealing with is not in the list, the field should not be populated."

	var ecsHostOsType string
	if semConvOsType, exists := resource.Attributes().Get(string(semconv.OSTypeKey)); exists {
		switch semConvOsType.Str() {
		case "windows", "linux":
			ecsHostOsType = semConvOsType.Str()
		case "darwin":
			ecsHostOsType = "macos"
		case "aix", "hpux", "solaris":
			ecsHostOsType = "unix"
		}
	}

	if semConvOsName, exists := resource.Attributes().Get(string(semconv.OSNameKey)); exists {
		switch semConvOsName.Str() {
		case "Android":
			ecsHostOsType = "android"
		case "iOS":
			ecsHostOsType = "ios"
		}
	}

	if ecsHostOsType == "" {
		return
	}
	document.AddString("host.os.type", ecsHostOsType)
}

func encodeLogTimestampECSMode(document *objmodel.Document, record plog.LogRecord) {
	if record.Timestamp() != 0 {
		document.AddTimestamp("@timestamp", record.Timestamp())
		return
	}

	document.AddTimestamp("@timestamp", record.ObservedTimestamp())
}
