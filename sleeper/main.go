package main

import (
	"context"
	"encoding/json"
	"errors"
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
	handler := newHTTPHandler(tracer, meter)

	srv := &http.Server{
		Addr:         ":40691",
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

func newHTTPHandler(tracer trace.Tracer, meter metric.Meter) http.Handler {
	mux := http.NewServeMux()

	handleFunc := func(pattern string, handler http.Handler) {
		handler = otelhttp.WithRouteTag(pattern, handler)
		mux.Handle(pattern, handler)
	}

	handleFunc("POST /sleeper", sleeperHandler{tracer: tracer, meter: meter})

	handler := otelhttp.NewHandler(mux, "/")
	return handler
}

type sleeperHandler struct {
	tracer trace.Tracer
	meter  metric.Meter
}

func (h sleeperHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()

	ctx, span := h.tracer.Start(ctx, "sleeperHandler")
	defer span.End()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		span.SetStatus(codes.Error, "Failed to read request body")
		span.RecordError(err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	bag := baggage.FromContext(ctx)
	slog.Info("bag", "member", bag.String())

	var sleeperReq common.SleeperRequest
	if err = json.Unmarshal(bodyBytes, &sleeperReq); err != nil {
		span.SetStatus(codes.Error, "Failed to unmarshal request body")
		span.RecordError(err)
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	if err = h.sleep(ctx, sleeperReq); err != nil {
		span.SetStatus(codes.Error, "Failed to to sleep")
		span.RecordError(err)
		common.HandleError(rw, err)
		return
	}
}

func (h sleeperHandler) sleep(ctx context.Context, req common.SleeperRequest) error {
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
	case <-time.After(time.Duration(req.SleeperSleepFor)):
		span.AddEvent("Sleep operation completed")
	}
	return nil
}

const (
	instrumentationScope = "github.com/mat-sik/open-telemetry-example/sleeper"
	serviceName          = "sleeper"
	exporterHost         = "localhost:4317"
)
