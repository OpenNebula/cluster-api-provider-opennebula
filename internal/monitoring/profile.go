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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	APIVersion              = "monitoring.oneks.opennebula.io/v1alpha1"
	Kind                    = "MonitoringProfile"
	ProfileLabel            = "monitoring.oneks.opennebula.io/profile"
	ProfileDataKey          = "profile.yaml"
	MaxProfileDocumentBytes = 65536
	MaxProfiles             = 32
	MaxSources              = 8
	MaxRules                = 64
	MaxSeries               = 100
	MaxQueryBytes           = 4096
	MaxMessageBytes         = 512
	MaxSignalLabels         = 8
	MinEvaluationInterval   = 15 * time.Second
	MaxEvaluationInterval   = 24 * time.Hour
	MinSourceTimeout        = time.Second
	MaxSourceTimeout        = 30 * time.Second
)

var (
	labelNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	templatePlaceholder = regexp.MustCompile(`{{\s*([A-Za-z]+)\s*}}`)
	allowedPlaceholders = map[string]struct{}{
		"value": {}, "threshold": {}, "severity": {}, "profile": {}, "rule": {},
	}
)

type Profile struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   ProfileMetadata `json:"metadata"`
	Spec       ProfileSpec     `json:"spec"`
}

type ProfileMetadata struct {
	Name string `json:"name"`
}

type ProfileSpec struct {
	EvaluationInterval Duration `json:"evaluationInterval"`
	Sources            []Source `json:"sources"`
	Rules              []Rule   `json:"rules"`
}

type Source struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Service ServiceReference `json:"service"`
	Timeout Duration         `json:"timeout"`
}

type ServiceReference struct {
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Port      ServicePort `json:"port"`
	Path      string      `json:"path"`
}

type Rule struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	Query      string     `json:"query"`
	Unit       string     `json:"unit"`
	Comparison Comparison `json:"comparison"`
	Warning    Number     `json:"warning"`
	Critical   Number     `json:"critical"`
	Recovery   Number     `json:"recovery"`
	Labels     RuleLabels `json:"labels"`
	Message    string     `json:"message"`
}

type RuleLabels struct {
	Allow []string `json:"allow"`
}

type Comparison string

const (
	GreaterThan        Comparison = "GreaterThan"
	GreaterThanOrEqual Comparison = "GreaterThanOrEqual"
	LessThan           Comparison = "LessThan"
	LessThanOrEqual    Comparison = "LessThanOrEqual"
)

type Duration time.Duration

type Number struct {
	value float64
	set   bool
}

func (n *Number) UnmarshalJSON(data []byte) error {
	var value float64
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("must be a number")
	}
	if !finite(value) {
		return fmt.Errorf("must be finite")
	}
	n.value = value
	n.set = true
	return nil
}

func (n Number) Value() float64 { return n.value }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("must be a duration string")
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q", raw)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

type ServicePort string

func (p *ServicePort) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		if validation.IsValidPortName(name) == nil {
			*p = ServicePort(name)
			return nil
		}
		return fmt.Errorf("must be a valid service port name or number")
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return fmt.Errorf("must be a valid service port name or number")
	}
	value, err := strconv.Atoi(number.String())
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("must be a valid service port name or number")
	}
	*p = ServicePort(strconv.Itoa(value))
	return nil
}

func ParseProfile(document []byte, allowedNamespaces map[string]struct{}) (Profile, error) {
	if len(document) == 0 {
		return Profile{}, fmt.Errorf("profile document is empty")
	}
	if len(document) > MaxProfileDocumentBytes {
		return Profile{}, fmt.Errorf("profile document exceeds %d bytes", MaxProfileDocumentBytes)
	}
	if err := validateYAMLStructure(document); err != nil {
		return Profile{}, err
	}
	jsonDocument, err := yaml.YAMLToJSON(document)
	if err != nil {
		return Profile{}, fmt.Errorf("decode profile YAML: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonDocument))
	decoder.DisallowUnknownFields()
	var profile Profile
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, fmt.Errorf("decode profile: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Profile{}, err
	}
	if err := validateProfile(profile, allowedNamespaces); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func validateYAMLStructure(document []byte) error {
	decoder := yamlv3.NewDecoder(bytes.NewReader(document))
	var root yamlv3.Node
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("decode profile YAML: %w", err)
	}
	var extra yamlv3.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("profile must contain exactly one YAML document")
		}
		return fmt.Errorf("decode profile YAML: %w", err)
	}
	if err := validateYAMLNode(&root); err != nil {
		return err
	}
	return nil
}

func validateYAMLNode(node *yamlv3.Node) error {
	if node.Kind == yamlv3.AliasNode {
		return fmt.Errorf("profile YAML aliases are not supported")
	}
	if node.Kind == yamlv3.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yamlv3.ScalarNode {
				return fmt.Errorf("profile YAML mapping keys must be scalars")
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("profile YAML contains duplicate key %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("profile must contain exactly one document")
		}
		return fmt.Errorf("decode profile: %w", err)
	}
	return nil
}

func validateProfile(profile Profile, allowedNamespaces map[string]struct{}) error {
	if profile.APIVersion != APIVersion || profile.Kind != Kind {
		return fmt.Errorf("profile must use apiVersion %q and kind %q", APIVersion, Kind)
	}
	if errors := validation.IsDNS1123Label(profile.Metadata.Name); len(errors) != 0 {
		return fmt.Errorf("metadata.name must be a DNS label: %s", strings.Join(errors, ", "))
	}
	interval := profile.Spec.EvaluationInterval.Value()
	if interval < MinEvaluationInterval || interval > MaxEvaluationInterval {
		return fmt.Errorf("evaluationInterval must be between %s and %s", MinEvaluationInterval, MaxEvaluationInterval)
	}
	if len(profile.Spec.Sources) == 0 || len(profile.Spec.Sources) > MaxSources {
		return fmt.Errorf("sources must contain between 1 and %d entries", MaxSources)
	}
	if len(profile.Spec.Rules) == 0 || len(profile.Spec.Rules) > MaxRules {
		return fmt.Errorf("rules must contain between 1 and %d entries", MaxRules)
	}
	sources := make(map[string]struct{}, len(profile.Spec.Sources))
	for _, source := range profile.Spec.Sources {
		if err := validateSource(source, interval, allowedNamespaces); err != nil {
			return fmt.Errorf("source %q: %w", source.ID, err)
		}
		if _, exists := sources[source.ID]; exists {
			return fmt.Errorf("duplicate source id %q", source.ID)
		}
		sources[source.ID] = struct{}{}
	}
	rules := make(map[string]struct{}, len(profile.Spec.Rules))
	for _, rule := range profile.Spec.Rules {
		if _, exists := rules[rule.ID]; exists {
			return fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		rules[rule.ID] = struct{}{}
		if _, exists := sources[rule.Source]; !exists {
			return fmt.Errorf("rule %q references unknown source %q", rule.ID, rule.Source)
		}
		if err := validateRule(rule); err != nil {
			return fmt.Errorf("rule %q: %w", rule.ID, err)
		}
	}
	return nil
}

func validateSource(source Source, interval time.Duration, allowedNamespaces map[string]struct{}) error {
	if errors := validation.IsDNS1123Label(source.ID); len(errors) != 0 {
		return fmt.Errorf("id must be a DNS label")
	}
	if source.Type != "prometheus" {
		return fmt.Errorf("unsupported type %q", source.Type)
	}
	if errors := validation.IsDNS1123Label(source.Service.Namespace); len(errors) != 0 {
		return fmt.Errorf("service namespace must be a DNS label")
	}
	if _, allowed := allowedNamespaces[source.Service.Namespace]; !allowed {
		return fmt.Errorf("service namespace %q is not allowed", source.Service.Namespace)
	}
	if errors := validation.IsDNS1123Label(source.Service.Name); len(errors) != 0 {
		return fmt.Errorf("service name must be a DNS label")
	}
	if source.Service.Port == "" {
		return fmt.Errorf("service port is required")
	}
	parsedPath, err := url.ParseRequestURI(source.Service.Path)
	if err != nil || !strings.HasPrefix(source.Service.Path, "/") || parsedPath.IsAbs() || parsedPath.Host != "" || parsedPath.RawQuery != "" || parsedPath.Fragment != "" || len(source.Service.Path) > 256 {
		return fmt.Errorf("service path must be an absolute path without a host, query, or fragment")
	}
	timeout := source.Timeout.Value()
	if timeout < MinSourceTimeout || timeout > MaxSourceTimeout || timeout > interval {
		return fmt.Errorf("timeout must be between %s and %s and no greater than evaluationInterval", MinSourceTimeout, MaxSourceTimeout)
	}
	return nil
}

func validateRule(rule Rule) error {
	if errors := validation.IsDNS1123Label(rule.ID); len(errors) != 0 {
		return fmt.Errorf("id must be a DNS label")
	}
	if len(rule.Query) == 0 || len(rule.Query) > MaxQueryBytes {
		return fmt.Errorf("query must contain between 1 and %d bytes", MaxQueryBytes)
	}
	if len(rule.Unit) > 128 {
		return fmt.Errorf("unit exceeds 128 bytes")
	}
	if !rule.Warning.set || !rule.Critical.set || !rule.Recovery.set {
		return fmt.Errorf("warning, critical, and recovery thresholds are required")
	}
	warning, critical, recovery := rule.Warning.Value(), rule.Critical.Value(), rule.Recovery.Value()
	switch rule.Comparison {
	case GreaterThan, GreaterThanOrEqual:
		if critical < warning || recovery > warning {
			return fmt.Errorf("GreaterThan thresholds require recovery <= warning <= critical")
		}
	case LessThan, LessThanOrEqual:
		if critical > warning || recovery < warning {
			return fmt.Errorf("LessThan thresholds require critical <= warning <= recovery")
		}
	default:
		return fmt.Errorf("unsupported comparison %q", rule.Comparison)
	}
	if len(rule.Labels.Allow) > MaxSignalLabels {
		return fmt.Errorf("labels.allow exceeds %d entries", MaxSignalLabels)
	}
	seenLabels := make(map[string]struct{}, len(rule.Labels.Allow))
	for _, label := range rule.Labels.Allow {
		if len(label) == 0 || len(label) > 63 || !labelNamePattern.MatchString(label) {
			return fmt.Errorf("invalid allowed label %q", label)
		}
		if _, exists := seenLabels[label]; exists {
			return fmt.Errorf("duplicate allowed label %q", label)
		}
		seenLabels[label] = struct{}{}
	}
	if len(rule.Message) > MaxMessageBytes {
		return fmt.Errorf("message exceeds %d bytes", MaxMessageBytes)
	}
	if err := validateTemplate(rule.Message); err != nil {
		return err
	}
	return nil
}

func validateTemplate(template string) error {
	for _, match := range templatePlaceholder.FindAllStringSubmatch(template, -1) {
		if _, allowed := allowedPlaceholders[match[1]]; !allowed {
			return fmt.Errorf("message uses unsupported placeholder %q", match[1])
		}
	}
	remaining := templatePlaceholder.ReplaceAllString(template, "")
	if strings.Contains(remaining, "{{") || strings.Contains(remaining, "}}") {
		return fmt.Errorf("message contains malformed placeholder")
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
