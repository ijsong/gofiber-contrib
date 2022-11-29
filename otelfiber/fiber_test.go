package otelfiber

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	b3prop "go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/oteltest"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestChildSpanFromGlobalTracer(t *testing.T) {
	otel.SetTracerProvider(oteltest.NewTracerProvider())

	var gotSpan oteltrace.Span

	app := fiber.New()
	app.Use(Middleware("foobar"))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/user/123", nil))

	_, ok := gotSpan.(*oteltest.Span)
	assert.True(t, ok)
}

func TestChildSpanFromCustomTracer(t *testing.T) {
	provider := oteltest.NewTracerProvider()
	var gotSpan oteltrace.Span

	app := fiber.New()
	app.Use(Middleware("foobar", WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/user/123", nil))

	_, ok := gotSpan.(*oteltest.Span)
	assert.True(t, ok)
}

func TestTrace200(t *testing.T) {
	sr := new(oteltest.SpanRecorder)
	provider := oteltest.NewTracerProvider(oteltest.WithSpanRecorder(sr))

	var gotSpan oteltrace.Span

	app := fiber.New()
	app.Use(Middleware("foobar", WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		id := ctx.Params("id")
		return ctx.SendString(id)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/user/123", nil), 3000)

	// do and verify the request
	require.Equal(t, http.StatusOK, resp.StatusCode)

	mspan, ok := gotSpan.(*oteltest.Span)
	require.True(t, ok)
	assert.Equal(t, attribute.StringValue("foobar"), mspan.Attributes()[semconv.HTTPServerNameKey])

	// verify traces look good
	spans := sr.Completed()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "/user/:id", span.Name())
	assert.Equal(t, oteltrace.SpanKindServer, span.SpanKind())
	assert.Equal(t, attribute.StringValue("foobar"), span.Attributes()["http.server_name"])
	assert.Equal(t, attribute.IntValue(http.StatusOK), span.Attributes()["http.status_code"])
	assert.Equal(t, attribute.StringValue("GET"), span.Attributes()["http.method"])
	assert.Equal(t, attribute.StringValue("/user/123"), span.Attributes()["http.target"])
	assert.Equal(t, attribute.StringValue("/user/:id"), span.Attributes()["http.route"])
}

func TestError(t *testing.T) {
	sr := new(oteltest.SpanRecorder)
	provider := oteltest.NewTracerProvider(oteltest.WithSpanRecorder(sr))

	// setup
	app := fiber.New()
	app.Use(Middleware("foobar", WithTracerProvider(provider)))
	// configure a handler that returns an error and 5xx status
	// code
	app.Get("/server_err", func(ctx *fiber.Ctx) error {
		return errors.New("oh no")
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/server_err", nil))
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// verify the errors and status are correct
	spans := sr.Completed()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "/server_err", span.Name())
	assert.Equal(t, attribute.StringValue("foobar"), span.Attributes()["http.server_name"])
	assert.Equal(t, attribute.IntValue(http.StatusInternalServerError), span.Attributes()["http.status_code"])
	assert.Equal(t, attribute.StringValue("oh no"), span.Events()[0].Attributes[semconv.ExceptionMessageKey])
	// server errors set the status
	assert.Equal(t, codes.Error, span.StatusCode())
}

func TestErrorOnlyHandledOnce(t *testing.T) {
	timesHandlingError := 0
	app := fiber.New(fiber.Config{
		ErrorHandler: func(ctx *fiber.Ctx, err error) error {
			timesHandlingError++
			return fiber.NewError(http.StatusInternalServerError, err.Error())
		},
	})
	app.Use(Middleware("test-service"))
	app.Get("/", func(ctx *fiber.Ctx) error {
		return errors.New("mock error")
	})
	_, _ = app.Test(httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, 1, timesHandlingError)
}

func TestGetSpanNotInstrumented(t *testing.T) {
	var gotSpan oteltrace.Span

	app := fiber.New()
	app.Get("/ping", func(ctx *fiber.Ctx) error {
		// Assert we don't have a span on the context.
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		return ctx.SendString("ok")
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/ping", nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	ok := !gotSpan.SpanContext().IsValid()
	assert.True(t, ok)
}

func TestPropagationWithGlobalPropagators(t *testing.T) {
	sr := new(oteltest.SpanRecorder)
	provider := oteltest.NewTracerProvider(oteltest.WithSpanRecorder(sr))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	var gotSpan oteltrace.Span

	r := httptest.NewRequest("GET", "/user/123", nil)

	ctx, pspan := provider.Tracer(instrumentationName).Start(context.Background(), "test")
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))

	app := fiber.New()
	app.Use(Middleware("foobar", WithTracerProvider(provider)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(r)

	mspan, ok := gotSpan.(*oteltest.Span)
	require.True(t, ok)
	assert.Equal(t, pspan.SpanContext().TraceID(), mspan.SpanContext().TraceID())
	assert.Equal(t, pspan.SpanContext().SpanID(), mspan.ParentSpanID())
}

func TestPropagationWithCustomPropagators(t *testing.T) {
	sr := new(oteltest.SpanRecorder)
	provider := oteltest.NewTracerProvider(oteltest.WithSpanRecorder(sr))
	var gotSpan oteltrace.Span

	b3 := b3prop.New()

	r := httptest.NewRequest("GET", "/user/123", nil)

	ctx, pspan := provider.Tracer(instrumentationName).Start(context.Background(), "test")
	b3.Inject(ctx, propagation.HeaderCarrier(r.Header))

	app := fiber.New()
	app.Use(Middleware("foobar", WithTracerProvider(provider), WithPropagators(b3)))
	app.Get("/user/:id", func(ctx *fiber.Ctx) error {
		gotSpan = oteltrace.SpanFromContext(ctx.UserContext())
		return ctx.SendStatus(http.StatusNoContent)
	})

	_, _ = app.Test(r)

	mspan, ok := gotSpan.(*oteltest.Span)
	require.True(t, ok)
	assert.Equal(t, pspan.SpanContext().TraceID(), mspan.SpanContext().TraceID())
	assert.Equal(t, pspan.SpanContext().SpanID(), mspan.ParentSpanID())
}

func TestHasBasicAuth(t *testing.T) {
	testCases := []struct {
		desc  string
		auth  string
		user  string
		valid bool
	}{
		{
			desc:  "valid header",
			auth:  "Basic dXNlcjpwYXNzd29yZA==",
			user:  "user",
			valid: true,
		},
		{
			desc: "invalid header",
			auth: "Bas",
		},
		{
			desc: "invalid basic header",
			auth: "Basic 12345",
		},
		{
			desc: "no header",
		},
	}

	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {

			val, valid := hasBasicAuth(tC.auth)

			assert.Equal(t, tC.user, val)
			assert.Equal(t, tC.valid, valid)
		})
	}
}

func TestMetric(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	app := fiber.New()
	app.Use(Middleware("foobar", WithMeterProvider(provider)))
	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(http.StatusOK)
	})
	_, _ = app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	metrics, err := reader.Collect(context.Background())
	assert.NoError(t, err)
	assert.Len(t, metrics.ScopeMetrics, 1)
	assert.Equal(t, instrumentationName, metrics.ScopeMetrics[0].Scope.Name)
}
