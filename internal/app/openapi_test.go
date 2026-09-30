package app

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPISpecIsValid(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(openapiSpec, &doc); err != nil {
		t.Fatalf("openapi.yaml 不是合法 YAML: %v", err)
	}
	if doc["openapi"] == nil {
		t.Fatal("缺少 openapi 版本字段")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("paths 为空")
	}
	// 路由表里的每个 /v1 接口都应能在 OpenAPI 中找到声明（除个别通配/页面路由）
	expected := []string{
		"/v1/platform/orders", "/v1/platform/orders/{id}/cancel",
		"/v1/platform/orders/{id}/redispatch",
		"/v1/staff/cups/{id}/seal", "/v1/staff/cups/{id}/remake",
		"/v1/rider/handoffs", "/v1/rider/arrivals", "/v1/rider/screen",
		"/v1/rider/arrivals/{id}/leave",
		"/v1/orders/{id}/wait-attribution", "/v1/orders/{id}/wait-appeals",
		"/v1/stores/{id}/wait-thresholds", "/v1/stores/{id}/wait-appeals",
		"/v1/wait-appeals/{id}/review",
		"/v1/customers/orders/{id}/complain", "/v1/finance/exceptions/{id}",
		"/v1/reports/region", "/v1/management/stores/{id}/reviews",
		"/v1/staff/stores/{id}/surge", "/v1/staff/merge",
	}
	for _, p := range expected {
		if _, ok := paths[p]; !ok {
			t.Errorf("OpenAPI 缺少路径声明: %s", p)
		}
	}
}
