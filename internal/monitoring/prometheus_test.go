/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitoring

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type queryRoundTripper func(*http.Request) (*http.Response, error)

func (f queryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPrometheusQueryResolvesKubernetesService(t *testing.T) {
	var method, host, path, query string
	client := queryClient(t, queryRoundTripper(func(request *http.Request) (*http.Response, error) {
		method, host, path = request.Method, request.URL.Host, request.URL.Path
		query = request.URL.Query().Get("query")
		return prometheusHTTPResponse(http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"worker-1","zone":"a"},"value":[1720000000,"81.5"]}]}}`), nil
	}))

	samples, err := client.Query(context.Background(), testSource(), "node_memory_pressure > 0")
	if err != nil {
		t.Fatalf("query Prometheus: %v", err)
	}
	if method != http.MethodGet || host != "prometheus.monitoring.svc:9090" || path != "/api/v1/query" || query != "node_memory_pressure > 0" {
		t.Fatalf("unexpected request: method=%s host=%s path=%s query=%s", method, host, path, query)
	}
	if len(samples) != 1 || samples[0].Value != 81.5 || samples[0].Labels["zone"] != "a" {
		t.Fatalf("unexpected samples: %#v", samples)
	}
}

func TestPrometheusQueryParsesScalar(t *testing.T) {
	client := queryClient(t, staticPrometheusResponse(`{"status":"success","data":{"resultType":"scalar","result":[1720000000,"3"]}}`))
	samples, err := client.Query(context.Background(), testSource(), "count(up)")
	if err != nil || len(samples) != 1 || samples[0].Value != 3 {
		t.Fatalf("unexpected scalar result: samples=%#v error=%v", samples, err)
	}
}

func TestPrometheusQueryFailuresAreBounded(t *testing.T) {
	tests := map[string]queryRoundTripper{
		"http failure": func(_ *http.Request) (*http.Response, error) {
			return prometheusHTTPResponse(http.StatusBadGateway, "sensitive backend response"), nil
		},
		"malformed JSON":    staticPrometheusResponse(`{"status":`),
		"matrix result":     staticPrometheusResponse(`{"status":"success","data":{"resultType":"matrix","result":[]}}`),
		"non-finite value":  staticPrometheusResponse(`{"status":"success","data":{"resultType":"scalar","result":[1,"NaN"]}}`),
		"transport failure": func(_ *http.Request) (*http.Response, error) { return nil, errors.New("connection refused") },
	}
	for name, transport := range tests {
		t.Run(name, func(t *testing.T) {
			client := queryClient(t, transport)
			_, err := client.Query(context.Background(), testSource(), "up")
			if err == nil {
				t.Fatal("expected query failure")
			}
			if strings.Contains(err.Error(), "sensitive backend response") {
				t.Fatalf("response body leaked in error: %v", err)
			}
		})
	}
}

func TestPrometheusQueryHonorsContextCancellation(t *testing.T) {
	client := queryClient(t, queryRoundTripper(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := client.Query(ctx, testSource(), "up")
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestPrometheusQueryRejectsExcessCardinality(t *testing.T) {
	series := make([]string, MaxSeries+1)
	for index := range series {
		series[index] = `{"metric":{},"value":[1,"1"]}`
	}
	body := `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`
	client := queryClient(t, staticPrometheusResponse(body))
	_, err := client.Query(context.Background(), testSource(), "up")
	if err == nil || !strings.Contains(err.Error(), "exceeds 100 series") {
		t.Fatalf("expected cardinality error, got %v", err)
	}
}

func TestPrometheusQueryRequiresConfiguredServicePort(t *testing.T) {
	source := testSource()
	source.Service.Port = "missing"
	client := queryClient(t, staticPrometheusResponse(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	_, err := client.Query(context.Background(), source, "up")
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("expected service port error, got %v", err)
	}
}

func TestPrometheusQueryDoesNotFollowRedirects(t *testing.T) {
	requests := 0
	client := queryClient(t, queryRoundTripper(func(_ *http.Request) (*http.Response, error) {
		requests++
		response := prometheusHTTPResponse(http.StatusFound, "")
		response.Header.Set("Location", "https://external.example/collect")
		return response, nil
	}))
	_, err := client.Query(context.Background(), testSource(), "up")
	if err == nil || requests != 1 {
		t.Fatalf("redirect was followed: requests=%d error=%v", requests, err)
	}
}

func TestPrometheusQueryRejectsExternalNameService(t *testing.T) {
	kubernetes := fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus", Namespace: "monitoring"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeExternalName, ExternalName: "external.example",
			Ports: []corev1.ServicePort{{Name: "http", Port: 9090}},
		},
	})
	client := NewPrometheusClient(kubernetes, &http.Client{Transport: staticPrometheusResponse(`{"status":"success"}`)})
	_, err := client.Query(context.Background(), testSource(), "up")
	if err == nil || !strings.Contains(err.Error(), "ExternalName") {
		t.Fatalf("expected ExternalName rejection, got %v", err)
	}
}

func queryClient(t *testing.T, transport http.RoundTripper) *PrometheusClient {
	t.Helper()
	kubernetes := fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus", Namespace: "monitoring"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 9090}}},
	})
	return NewPrometheusClient(kubernetes, &http.Client{Transport: transport})
}

func testSource() Source {
	return Source{
		ID: "prometheus", Type: "prometheus",
		Service: ServiceReference{
			Namespace: "monitoring", Name: "prometheus",
			Port: "http", Path: "/api/v1/query",
		},
		Timeout: Duration(10 * time.Second),
	}
}

func staticPrometheusResponse(body string) queryRoundTripper {
	return func(_ *http.Request) (*http.Response, error) {
		return prometheusHTTPResponse(http.StatusOK, body), nil
	}
}

func prometheusHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
