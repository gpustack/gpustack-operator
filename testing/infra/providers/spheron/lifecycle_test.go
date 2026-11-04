package main

import (
	"context"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// This uses the real Terraform CLI and provider protocol with an injected API.
func TestTerraformLifecycle(t *testing.T) {
	var mu sync.Mutex
	creates, deletes := 0, 0
	var deployment map[string]any
	call := apiCall(func(_ context.Context, operation string, input map[string]any) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch operation {
		case "create":
			creates++
			deployment = map[string]any{
				"id": "offline-deployment", "status": "running", "ipAddress": "192.0.2.10",
				"user": "ubuntu", "sshPort": float64(22), "sshCommand": "ssh ubuntu@192.0.2.10",
			}
			for key, value := range input["request"].(map[string]any) {
				deployment[key] = value
			}
			deployment["providerId"] = deployment["provider"]
		case "delete":
			deletes++
			deployment["status"] = "terminated"
		}
		if deployment == nil {
			return nil, &apiError{Status: 404, Code: "not_found"}
		}
		out := map[string]any{}
		for key, value := range deployment {
			out[key] = value
		}
		return out, nil
	})
	const config = `
resource "spheron_instance" "test" {
  request_key = "offline-allocation-001"
  request_json = jsonencode({
    provider = "spheron-es", offerId = "fixture", gpuType = "h100", gpuCount = 8,
    region = "fixture", operatingSystem = "Ubuntu", instanceType = "SPOT", sshKeyId = "fixture"
  })
}`
	resource.UnitTest(t, resource.TestCase{
		ProviderFactories: map[string]func() (*schema.Provider, error){
			"spheron": func() (*schema.Provider, error) {
				p := provider()
				p.ConfigureContextFunc = func(context.Context, *schema.ResourceData) (any, diag.Diagnostics) {
					return call, nil
				}
				return p, nil
			},
		},
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("spheron_instance.test", "id", "offline-deployment"),
				resource.TestCheckResourceAttr("spheron_instance.test", "ssh_port", "22"),
			)},
			{Config: config, PlanOnly: true},
			{
				ResourceName: "spheron_instance.test", ImportState: true, ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{"request_key"},
			},
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 || deletes != 1 {
		t.Fatalf("unexpected mutation count: creates=%d deletes=%d", creates, deletes)
	}
}
