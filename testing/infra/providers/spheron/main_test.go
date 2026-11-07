package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func fixture(t *testing.T) *schema.ResourceData {
	t.Helper()
	return schema.TestResourceDataRaw(t, instance().Schema, map[string]any{
		"request_key":  "allocation-001",
		"request_json": `{"provider":"spheron-es","offerId":"offer","gpuType":"h100","gpuCount":8,"region":"region","operatingSystem":"Ubuntu","instanceType":"SPOT","sshKeyId":"key"}`,
	})
}

func TestSchema(t *testing.T) {
	if err := provider().InternalValidate(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRetainsIdentityAfterWaitFailure(t *testing.T) {
	d := fixture(t)
	calls := []string{}
	call := apiCall(func(_ context.Context, operation string, _ map[string]any) (map[string]any, error) {
		calls = append(calls, operation)
		if operation == "create" {
			return map[string]any{"id": "deployment-1"}, nil
		}
		return nil, errors.New("wait failed")
	})
	if diagnostics := createInstance(context.Background(), d, call); !diagnostics.HasError() {
		t.Fatal("expected wait error")
	}
	if d.Id() != "deployment-1" || len(calls) != 2 || calls[1] != "wait" {
		t.Fatalf("lost identity or unexpected operations: id=%q calls=%v", d.Id(), calls)
	}
}

func TestReadKeepsUnknownAndStoppedResources(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
		removed      bool
	}{
		{name: "running", status: "running"},
		{name: "stopped", status: "stopped"},
		{name: "failed", status: "failed"},
		{name: "deleted", status: "terminated", removed: true},
		{name: "preempted", status: "terminated-provider", removed: true},
		{name: "absent", err: &apiError{Status: 404}, removed: true},
		{name: "forbidden", err: &apiError{Status: 403}},
		{name: "server error", err: &apiError{Status: 503}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			d.SetId("deployment-1")
			call := apiCall(func(context.Context, string, map[string]any) (map[string]any, error) {
				return map[string]any{"id": "deployment-1", "status": tc.status}, tc.err
			})
			diagnostics := readInstance(context.Background(), d, call)
			if (d.Id() == "") != tc.removed {
				t.Fatalf("unexpected state ID %q", d.Id())
			}
			if diagnostics.HasError() != (tc.err != nil && !tc.removed) {
				t.Fatalf("unexpected diagnostics: %v", diagnostics)
			}
		})
	}
}

func TestDeleteRetainsIdentityUntilConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name, failAt string
		removed      bool
	}{
		{name: "minimum runtime", failAt: "delete"},
		{name: "timeout", failAt: "wait"},
		{name: "confirmed", removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			d.SetId("deployment-1")
			call := apiCall(func(_ context.Context, operation string, input map[string]any) (map[string]any, error) {
				if input["id"] != "deployment-1" {
					t.Fatalf("unexpected delete identity %v", input)
				}
				if operation == tc.failAt {
					return nil, errors.New("not confirmed")
				}
				return map[string]any{"id": "deployment-1", "status": "terminated"}, nil
			})
			diagnostics := deleteInstance(context.Background(), d, call)
			if (d.Id() == "") != tc.removed || diagnostics.HasError() == tc.removed {
				t.Fatalf("id=%q diagnostics=%v", d.Id(), diagnostics)
			}
		})
	}
}

func TestEmbeddedPythonReportsMissingCredential(t *testing.T) {
	t.Setenv("SPHERON_API_KEY", "")
	_, err := bridge("python3")(context.Background(), "get", map[string]any{"id": "fixture"})
	var failure *apiError
	if !errors.As(err, &failure) || failure.Code != "credentials_missing" {
		t.Fatalf("unexpected embedded client error: %v", err)
	}
}
