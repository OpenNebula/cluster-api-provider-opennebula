/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const MaxPrometheusResponseBytes = 1 << 20

type Sample struct {
	Value  float64
	Labels map[string]string
}

type PrometheusClient struct {
	kubernetes kubernetes.Interface
	http       *http.Client
}

func NewPrometheusClient(kubernetesClient kubernetes.Interface, httpClient *http.Client) *PrometheusClient {
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		httpClient = &http.Client{Transport: transport}
	}
	client := *httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &PrometheusClient{kubernetes: kubernetesClient, http: &client}
}

func (c *PrometheusClient) Query(ctx context.Context, source Source, query string) ([]Sample, error) {
	service, err := c.kubernetes.CoreV1().Services(source.Service.Namespace).Get(
		ctx, source.Service.Name, metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("resolve Prometheus Service: %w", err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return nil, fmt.Errorf("Prometheus ExternalName Services are not supported")
	}
	port, err := servicePort(service, source.Service.Port)
	if err != nil {
		return nil, err
	}
	endpoint := url.URL{
		Scheme: "http",
		Host: fmt.Sprintf(
			"%s.%s.svc:%d", source.Service.Name,
			source.Service.Namespace, port,
		),
		Path: source.Service.Path,
	}
	parameters := endpoint.Query()
	parameters.Set("query", query)
	endpoint.RawQuery = parameters.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Prometheus query request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query Prometheus Service: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Prometheus Service returned HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxPrometheusResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Prometheus response: %w", err)
	}
	if len(body) > MaxPrometheusResponseBytes {
		return nil, fmt.Errorf("Prometheus response exceeds %d bytes", MaxPrometheusResponseBytes)
	}
	return parsePrometheusResponse(body)
}

func servicePort(service *corev1.Service, configured ServicePort) (int32, error) {
	wanted := string(configured)
	if number, err := strconv.Atoi(wanted); err == nil {
		for _, port := range service.Spec.Ports {
			if port.Port == int32(number) {
				return port.Port, nil
			}
		}
	} else {
		for _, port := range service.Spec.Ports {
			if port.Name == wanted {
				return port.Port, nil
			}
		}
	}
	return 0, fmt.Errorf("Prometheus Service port %q was not found", wanted)
}

type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

type prometheusVectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  json.RawMessage   `json:"value"`
}

func parsePrometheusResponse(body []byte) ([]Sample, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var response prometheusResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if response.Status != "success" {
		return nil, fmt.Errorf("Prometheus query did not succeed")
	}
	switch response.Data.ResultType {
	case "scalar":
		value, err := parsePrometheusValue(response.Data.Result)
		if err != nil {
			return nil, err
		}
		return []Sample{{Value: value, Labels: map[string]string{}}}, nil
	case "vector":
		var result []prometheusVectorSample
		if err := json.Unmarshal(response.Data.Result, &result); err != nil {
			return nil, fmt.Errorf("decode Prometheus vector: %w", err)
		}
		if len(result) > MaxSeries {
			return nil, fmt.Errorf("Prometheus result exceeds %d series", MaxSeries)
		}
		samples := make([]Sample, 0, len(result))
		for _, item := range result {
			value, err := parsePrometheusValue(item.Value)
			if err != nil {
				return nil, err
			}
			samples = append(samples, Sample{Value: value, Labels: item.Metric})
		}
		return samples, nil
	default:
		return nil, fmt.Errorf("unsupported Prometheus result type %q", response.Data.ResultType)
	}
}

func parsePrometheusValue(raw json.RawMessage) (float64, error) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return 0, fmt.Errorf("malformed Prometheus sample")
	}
	var encoded string
	if err := json.Unmarshal(tuple[1], &encoded); err != nil {
		return 0, fmt.Errorf("malformed Prometheus sample value")
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(encoded), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("Prometheus sample value must be finite")
	}
	return value, nil
}
