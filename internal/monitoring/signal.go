/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitoring

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SignalKind             = "ClusterSignal"
	SignalSourcePrometheus = "prometheus"
	SignalSourceKubernetes = "kubernetes"
	MaxSignalBytes         = 16384
	MaxSignalFieldBytes    = 128
	MaxSignalLabelKeyBytes = 63
	MaxSignalLabelValue    = 128
)

type ClusterSignal struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	ClusterID  string            `json:"clusterId"`
	Identity   string            `json:"identity"`
	Profile    string            `json:"profile"`
	Rule       string            `json:"rule"`
	Source     string            `json:"source"`
	Category   string            `json:"category"`
	Severity   string            `json:"severity"`
	Status     string            `json:"status"`
	ObservedAt string            `json:"observedAt"`
	Value      float64           `json:"value"`
	Unit       string            `json:"unit"`
	Threshold  float64           `json:"threshold"`
	Labels     map[string]string `json:"labels"`
	Message    string            `json:"message"`
}

func (ClusterSignal) CallbackKind() string { return SignalKind }

func NewClusterSignal(
	clusterID string, profile Profile, rule Rule, sample Sample,
	status, severity string, threshold float64, observedAt time.Time,
	messageOverride string,
) (ClusterSignal, error) {
	labels, err := signalLabels(rule, sample)
	if err != nil {
		return ClusterSignal{}, err
	}
	identity := signalIdentity(profile.Metadata.Name, rule.ID, SignalSourcePrometheus, labels)
	message := messageOverride
	if message == "" {
		message = renderMessage(rule.Message, map[string]string{
			"value":     formatNumber(sample.Value),
			"threshold": formatNumber(threshold),
			"severity":  severity,
			"profile":   profile.Metadata.Name,
			"rule":      rule.ID,
		})
	}
	signal := ClusterSignal{
		APIVersion: APIVersion,
		Kind:       SignalKind,
		ClusterID:  clusterID,
		Identity:   identity,
		Profile:    profile.Metadata.Name,
		Rule:       rule.ID,
		Source:     SignalSourcePrometheus,
		Category:   "monitoring",
		Severity:   severity,
		Status:     status,
		ObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
		Value:      sample.Value,
		Unit:       rule.Unit,
		Threshold:  threshold,
		Labels:     labels,
		Message:    truncateUTF8(message, MaxMessageBytes),
	}
	if err := signal.Validate(); err != nil {
		return ClusterSignal{}, err
	}
	return signal, nil
}

func signalLabels(rule Rule, sample Sample) (map[string]string, error) {
	labels := make(map[string]string, len(rule.Labels.Allow))
	for _, key := range rule.Labels.Allow {
		value, exists := sample.Labels[key]
		if !exists {
			continue
		}
		if len(value) > MaxSignalLabelValue || !utf8.ValidString(value) {
			return nil, fmt.Errorf("Prometheus label %q exceeds signal limits", key)
		}
		labels[key] = value
	}
	return labels, nil
}

func (s ClusterSignal) Validate() error {
	if s.APIVersion != APIVersion || s.Kind != SignalKind {
		return fmt.Errorf("invalid ClusterSignal apiVersion or kind")
	}
	for field, value := range map[string]string{
		"clusterId": s.ClusterID, "identity": s.Identity, "profile": s.Profile,
		"rule": s.Rule, "source": s.Source, "category": s.Category, "unit": s.Unit,
	} {
		if value == "" || len(value) > MaxSignalFieldBytes || !utf8.ValidString(value) {
			return fmt.Errorf("ClusterSignal %s must contain between 1 and %d bytes", field, MaxSignalFieldBytes)
		}
	}
	if !strings.HasPrefix(s.Identity, "signal-") {
		return fmt.Errorf("ClusterSignal identity is invalid")
	}
	if s.Source != SignalSourcePrometheus && s.Source != SignalSourceKubernetes {
		return fmt.Errorf("ClusterSignal source is invalid")
	}
	if s.Severity != "info" && s.Severity != "warning" && s.Severity != "critical" {
		return fmt.Errorf("ClusterSignal severity is invalid")
	}
	if s.Status != "active" && s.Status != "resolved" && s.Status != "unknown" {
		return fmt.Errorf("ClusterSignal status is invalid")
	}
	if _, err := time.Parse(time.RFC3339, s.ObservedAt); err != nil {
		return fmt.Errorf("ClusterSignal observedAt is invalid")
	}
	if !finite(s.Value) || !finite(s.Threshold) {
		return fmt.Errorf("ClusterSignal values must be finite")
	}
	if len(s.Labels) > MaxSignalLabels {
		return fmt.Errorf("ClusterSignal labels exceed %d entries", MaxSignalLabels)
	}
	for key, value := range s.Labels {
		if len(key) == 0 || len(key) > MaxSignalLabelKeyBytes || !labelNamePattern.MatchString(key) {
			return fmt.Errorf("ClusterSignal label key %q is invalid", key)
		}
		if len(value) > MaxSignalLabelValue || !utf8.ValidString(value) {
			return fmt.Errorf("ClusterSignal label value for %q is invalid", key)
		}
	}
	if len(s.Message) > MaxMessageBytes || !utf8.ValidString(s.Message) {
		return fmt.Errorf("ClusterSignal message is invalid")
	}
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode ClusterSignal: %w", err)
	}
	if len(body) > MaxSignalBytes {
		return fmt.Errorf("ClusterSignal exceeds %d bytes", MaxSignalBytes)
	}
	return nil
}

func signalIdentity(profile, rule, source string, labels map[string]string) string {
	canonical := canonicalSignalIdentityInput(profile, rule, source, labels)
	digest := sha256.Sum256([]byte(canonical))
	return "signal-" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func canonicalSignalIdentityInput(profile, rule, source string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	canonical.WriteString(`{"labels":{`)
	for index, key := range keys {
		if index != 0 {
			canonical.WriteByte(',')
		}
		canonical.WriteString(quoteASCIIJSON(key))
		canonical.WriteByte(':')
		canonical.WriteString(quoteASCIIJSON(labels[key]))
	}
	canonical.WriteString(`},"profile":`)
	canonical.WriteString(quoteASCIIJSON(profile))
	canonical.WriteString(`,"rule":`)
	canonical.WriteString(quoteASCIIJSON(rule))
	canonical.WriteString(`,"source":`)
	canonical.WriteString(quoteASCIIJSON(source))
	canonical.WriteByte('}')
	return canonical.String()
}

func quoteASCIIJSON(value string) string {
	encoded, _ := json.Marshal(value)
	var ascii strings.Builder
	ascii.Grow(len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] < 0x7f {
			ascii.WriteByte(encoded[index])
			index++
			continue
		}
		if encoded[index] == 0x7f {
			writeJSONUnicodeEscape(&ascii, 0x7f)
			index++
			continue
		}
		runeValue, size := utf8.DecodeRune(encoded[index:])
		if runeValue <= 0xffff {
			writeJSONUnicodeEscape(&ascii, uint16(runeValue))
		} else {
			value := runeValue - 0x10000
			writeJSONUnicodeEscape(&ascii, uint16(0xd800+(value>>10)))
			writeJSONUnicodeEscape(&ascii, uint16(0xdc00+(value&0x3ff)))
		}
		index += size
	}
	return ascii.String()
}

func writeJSONUnicodeEscape(builder *strings.Builder, value uint16) {
	const hexadecimal = "0123456789abcdef"
	builder.WriteString(`\u`)
	builder.WriteByte(hexadecimal[(value>>12)&0xf])
	builder.WriteByte(hexadecimal[(value>>8)&0xf])
	builder.WriteByte(hexadecimal[(value>>4)&0xf])
	builder.WriteByte(hexadecimal[value&0xf])
}

func renderMessage(template string, values map[string]string) string {
	return templatePlaceholder.ReplaceAllStringFunc(template, func(placeholder string) string {
		match := templatePlaceholder.FindStringSubmatch(placeholder)
		return values[match[1]]
	})
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
