package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mat-sik/open-telemetry-example/common"
	"github.com/mat-sik/open-telemetry-example/otel/setup"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := setup.InitOTelSDK(ctx, exporterHost, serviceName)
	if err != nil {
		slog.Error("Failed to initialize otel SDK", "err", err)
		return
	}
	defer func() {
		if err = shutdown(ctx); err != nil {
			slog.Error("Failed to shutdown otel SDK", "err", err)
		}
	}()

	tracer := otel.Tracer(instrumentationScope)
	meter := otel.Meter(instrumentationScope)

	logger := otelslog.NewLogger(instrumentationScope)
	slog.SetDefault(logger)

	runServer(ctx, tracer, meter)
}

func runServer(ctx context.Context, tracer trace.Tracer, meter metric.Meter) {
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	handler := newHTTPHandler(client, tracer, meter)

	srv := &http.Server{
		Addr:         ":40690",
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  time.Second,
		WriteTimeout: 10 * time.Second,
		Handler:      handler,
	}
	srvErr := make(chan error, 1)
	go func() {
		srvErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-srvErr:
		slog.Error("Received server error", "err", err)
	case <-ctx.Done():
		slog.Info("Shutting down server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			slog.Error("Server shutdown failed", "err", err)
		}
		slog.Info("Server shutdown complete")
	}
}

func newHTTPHandler(client *http.Client, tracer trace.Tracer, meter metric.Meter) http.Handler {
	mux := http.NewServeMux()

	handleFunc := func(pattern string, handler http.Handler) {
		handler = otelhttp.WithRouteTag(pattern, handler)
		mux.Handle(pattern, handler)
	}

	handleFunc("POST /gateway", gatewayHandler{client: client, tracer: tracer, meter: meter})

	handler := otelhttp.NewHandler(mux, "/")
	return handler
}

type gatewayHandler struct {
	client *http.Client
	tracer trace.Tracer
	meter  metric.Meter
}

func (h gatewayHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()

	ctx, span := h.tracer.Start(ctx, "gatewayHandler")
	defer span.End()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		span.SetStatus(codes.Error, "Failed to read request body")
		span.RecordError(err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	var sleeperReq common.SleeperRequest
	if err = json.Unmarshal(bodyBytes, &sleeperReq); err != nil {
		span.SetStatus(codes.Error, "Failed to unmarshal request body")
		span.RecordError(err)
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	bag, err := h.createBaggageFromQueryParams(ctx, req)
	if err != nil {
		span.SetStatus(codes.Error, "Failed to create baggage from query parameters")
		span.RecordError(err)
		common.HandleError(rw, err)
		return
	}

	ctx = baggage.ContextWithBaggage(ctx, bag)

	if err = h.sleep(ctx, sleeperReq); err != nil {
		span.SetStatus(codes.Error, "Failed to to sleep")
		span.RecordError(err)
		common.HandleError(rw, err)
		return
	}

	resp, err := h.remoteCallSleeper(ctx, bodyBytes)
	if err != nil {
		span.SetStatus(codes.Error, "Failed to to remote call sleeper")
		span.RecordError(err)
		common.HandleError(rw, err)
		return
	}
	defer func() {
		if err = resp.Body.Close(); err != nil {
			span.RecordError(err)
			slog.Error("failed to close response body", "err", err)
		}
	}()

	if err = h.handleResponse(ctx, rw, resp); err != nil {
		span.SetStatus(codes.Error, "Failed to to remote call sleeper")
		span.RecordError(err)
		return
	}
}

func (h gatewayHandler) createBaggageFromQueryParams(ctx context.Context, req *http.Request) (baggage.Baggage, error) {
	ctx, span := h.tracer.Start(ctx, "createBaggageFromQueryParameters")
	defer span.End()

	bag := baggage.FromContext(ctx)
	for key, values := range req.URL.Query() {
		value := strings.Join(values, ",")

		mem, err := baggage.NewMember(key, value)
		if err != nil {
			span.SetStatus(codes.Error, "Failed to create baggage member")
			span.RecordError(err)
			return baggage.Baggage{}, common.NewAppError(err, http.StatusBadRequest)
		}

		bag, err = bag.SetMember(mem)
		if err != nil {
			span.SetStatus(codes.Error, "Failed to set baggage member")
			span.RecordError(err)
			return baggage.Baggage{}, common.NewAppError(err, http.StatusBadRequest)
		}
	}

	return bag, nil
}

func (h gatewayHandler) sleep(ctx context.Context, req common.SleeperRequest) error {
	ctx, span := h.tracer.Start(ctx, "sleep")
	defer span.End()

	span.AddEvent("Starting sleep operation", trace.WithAttributes(
		attribute.String("sleep_duration", time.Duration(req.GatewaySleepFor).String()),
	))

	select {
	case <-ctx.Done():
		err := errors.New("request was running for too long or server got interrupted")
		span.SetStatus(codes.Error, "Sleep operation timeout or cancellation")
		span.RecordError(err)
		return common.NewAppError(err, http.StatusRequestTimeout)
	case <-time.After(time.Duration(req.GatewaySleepFor)):
		span.AddEvent("Sleep operation completed")
	}
	return nil
}

func (h gatewayHandler) remoteCallSleeper(ctx context.Context, bodyBytes []byte) (*http.Response, error) {
	targetUrl := fmt.Sprintf("http://%s/sleeper", otherServiceHost)
	clientReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetUrl, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, common.NewAppError(err, http.StatusInternalServerError)
	}

	resp, err := h.client.Do(clientReq)
	if err != nil {
		return nil, common.NewAppError(err, http.StatusInternalServerError)
	}

	return resp, err
}

func (h gatewayHandler) handleResponse(ctx context.Context, rw http.ResponseWriter, resp *http.Response) error {
	ctx, span := h.tracer.Start(ctx, "handleResponse")
	defer span.End()

	span.AddEvent("Sending response body to client")

	rw.WriteHeader(resp.StatusCode)

	n, err := io.Copy(rw, resp.Body)
	if err != nil {
		span.SetStatus(codes.Error, "Reading response body failed")
		span.RecordError(err, trace.WithAttributes(
			attribute.Int64("response.bytes_copied", n),
		))
		slog.Error("failed to copy response body", "copied bytes amount:", n, "err", err)
		return common.NewAppError(err, http.StatusInternalServerError)
	}

	span.AddEvent("Response body copied successfully", trace.WithAttributes(
		attribute.Int64("response.bytes_copied", n),
	))
	return nil
}

const (
	instrumentationScope = "github.com/mat-sik/open-telemetry-example/gateway"
	serviceName          = "gateway"
	exporterHost         = "localhost:4317"
	otherServiceHost     = "localhost:40691"
)
