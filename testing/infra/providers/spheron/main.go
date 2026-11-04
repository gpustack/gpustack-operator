package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-plugin-sdk/v2/plugin"
)

//go:embed python/spheron.py
var pythonSource string

type apiCall func(context.Context, string, map[string]any) (map[string]any, error)

type apiError struct {
	Code    string `json:"code"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

func bridge(python string) apiCall {
	return func(ctx context.Context, operation string, input map[string]any) (map[string]any, error) {
		body, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode API input: %w", err)
		}
		cmd := exec.CommandContext(ctx, python, "-c", pythonSource, operation)
		cmd.Stdin = strings.NewReader(string(body))
		output, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("API call interrupted; retain deployment identity: %w", ctx.Err())
			}
			exit := &exec.ExitError{}
			if errors.As(err, &exit) {
				var failure apiError
				if json.Unmarshal(exit.Stderr, &failure) == nil && failure.Code != "" {
					return nil, &failure
				}
			}
			return nil, fmt.Errorf("python API process failed; verify Python 3 and SPHERON_API_KEY")
		}
		var result map[string]any
		if err := json.Unmarshal(output, &result); err != nil || result == nil {
			return nil, fmt.Errorf("python API returned an invalid object")
		}
		return result, nil
	}
}

func provider() *schema.Provider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"python": {
				Type: schema.TypeString, Optional: true, Default: "python3",
				Description: "Python 3 executable. The embedded client reads SPHERON_API_KEY from its environment.",
			},
		},
		ResourcesMap: map[string]*schema.Resource{"spheron_instance": instance()},
		ConfigureContextFunc: func(_ context.Context, d *schema.ResourceData) (any, diag.Diagnostics) {
			return bridge(d.Get("python").(string)), nil
		},
	}
}

func instance() *schema.Resource {
	return &schema.Resource{
		Description:   "A Spheron VM or CPU deployment managed through the embedded Python client. Creation parameters are immutable.",
		CreateContext: createInstance,
		ReadContext:   readInstance,
		DeleteContext: deleteInstance,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(30 * time.Minute),
			Read:   schema.DefaultTimeout(time.Minute),
			Delete: schema.DefaultTimeout(30 * time.Minute),
		},
		Schema: map[string]*schema.Schema{
			"request_json": {
				Type: schema.TypeString, Required: true, ForceNew: true, Sensitive: true,
				ValidateFunc: validation.StringIsJSON,
				DiffSuppressFunc: func(_, oldValue, newValue string, _ *schema.ResourceData) bool {
					var a, b any
					if json.Unmarshal([]byte(oldValue), &a) != nil || json.Unmarshal([]byte(newValue), &b) != nil {
						return false
					}
					x, _ := json.Marshal(a)
					y, _ := json.Marshal(b)
					return string(x) == string(y)
				},
				Description: "JSON VM request using the documented Spheron field names. Stored in Terraform state; do not put secrets in cloudInit.",
			},
			"request_key": {
				Type: schema.TypeString, Required: true, ForceNew: true,
				ValidateFunc: validation.StringMatch(regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,200}$`), "request_key must contain 8-200 letters, digits, or _.:-"),
				Description:  "Unique Idempotency-Key. Reuse after an unknown create outcome; change for replacement or a changed request.",
			},
			"status":      {Type: schema.TypeString, Computed: true},
			"public_ip":   {Type: schema.TypeString, Computed: true},
			"ssh_user":    {Type: schema.TypeString, Computed: true},
			"ssh_port":    {Type: schema.TypeInt, Computed: true},
			"ssh_command": {Type: schema.TypeString, Computed: true},
		},
	}
}

func createInstance(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
	call := meta.(apiCall)
	var body map[string]any
	if err := json.Unmarshal([]byte(d.Get("request_json").(string)), &body); err != nil || body == nil {
		return diag.Errorf("request_json must be a JSON object")
	}
	result, err := call(ctx, "create", map[string]any{"request": body, "request_key": d.Get("request_key")})
	if err != nil {
		return diag.FromErr(err)
	}
	id, ok := result["id"].(string)
	if !ok || id == "" {
		return diag.Errorf("Create returned no deployment ID; reconcile using the same request_key before replacing it")
	}
	// Persist identity before waiting so failed provisioning still has a cleanup handle.
	d.SetId(id)
	result, err = call(ctx, "wait", map[string]any{"id": id, "target": "running", "timeout": d.Timeout(schema.TimeoutCreate).Seconds()})
	if err != nil {
		return diag.FromErr(err)
	}
	return saveInstance(d, result)
}

func readInstance(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
	result, err := meta.(apiCall)(ctx, "get", map[string]any{"id": d.Id()})
	if err != nil {
		failure := &apiError{}
		if errors.As(err, &failure) && failure.Status == 404 {
			d.SetId("")
			return nil
		}
		return diag.FromErr(err)
	}
	if result["status"] == "terminated" || result["status"] == "terminated-provider" {
		d.SetId("")
		return nil
	}
	// Imported instances have no creation request. Adopt their observed immutable fields.
	if d.Get("request_json").(string) == "" {
		body := map[string]any{}
		for _, key := range []string{"offerId", "gpuType", "gpuCount", "region", "operatingSystem", "instanceType", "sshKeyId", "teamId", "name"} {
			if value := result[key]; value != nil && value != "" {
				body[key] = value
			}
		}
		body["provider"] = result["providerId"]
		raw, err := json.Marshal(body)
		if err != nil {
			return diag.FromErr(err)
		}
		if err := d.Set("request_json", string(raw)); err != nil {
			return diag.FromErr(err)
		}
		if err := d.Set("request_key", "imported-"+d.Id()); err != nil {
			return diag.FromErr(err)
		}
	}
	return saveInstance(d, result)
}

func saveInstance(d *schema.ResourceData, result map[string]any) diag.Diagnostics {
	if result["id"] != d.Id() {
		return diag.Errorf("Deployment response identity differs from Terraform state")
	}
	for field, key := range map[string]string{"status": "status", "public_ip": "ipAddress", "ssh_user": "user", "ssh_command": "sshCommand"} {
		value, _ := result[key].(string)
		if err := d.Set(field, value); err != nil {
			return diag.FromErr(err)
		}
	}
	port := 0
	if value, ok := result["sshPort"].(float64); ok {
		port = int(value)
	}
	if err := d.Set("ssh_port", port); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func deleteInstance(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
	call := meta.(apiCall)
	if _, err := call(ctx, "delete", map[string]any{"id": d.Id()}); err != nil {
		return diag.FromErr(err)
	}
	waitInput := map[string]any{"id": d.Id(), "target": "terminated", "timeout": d.Timeout(schema.TimeoutDelete).Seconds()}
	if _, err := call(ctx, "wait", waitInput); err != nil {
		return diag.FromErr(err)
	}
	d.SetId("")
	return nil
}

func main() {
	plugin.Serve(&plugin.ServeOpts{ProviderFunc: provider})
}
