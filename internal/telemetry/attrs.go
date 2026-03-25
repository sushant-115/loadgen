package telemetry

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Convenience attribute helpers for metrics.

func AttrMethod(m string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("http.method", m))
}

func AttrPath(p string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("http.path", p))
}

func AttrStatus(code int) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("http.status_code", fmt.Sprintf("%d", code)))
}
