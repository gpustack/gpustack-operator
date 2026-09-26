package modelstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// clusterLayer is a layer that sets every field, as the Settings layer does.
func clusterLayer() Layer {
	return Layer{
		HighWatermarkPercent:   ptr.To[int32](80),
		LowWatermarkPercent:    ptr.To[int32](70),
		DownloadConcurrency:    ptr.To[int32](8),
		DownloadBytesPerSecond: ptr.To[int64](0),
		HuggingFaceEndpoint:    ptr.To("https://huggingface.co"),
		HTTPSProxy:             ptr.To(""),
		NoProxy:                ptr.To(""),
		CABundleConfigMap:      ptr.To(""),
	}
}

func validSpec() workercore.NodeModelStoreSpec {
	return Merge(clusterLayer())
}

func TestMerge(t *testing.T) {
	cases := []struct {
		name   string
		layers []Layer
		want   workercore.NodeModelStoreSpec
	}{
		{
			name:   "one full layer is the spec",
			layers: []Layer{clusterLayer()},
			want: workercore.NodeModelStoreSpec{
				Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 80, LowPercent: 70},
				Download:   workercore.NodeModelStoreDownload{Concurrency: 8},
				Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: "https://huggingface.co"},
			},
		},
		{
			name: "a second layer overrides only the fields it sets",
			layers: []Layer{clusterLayer(), {
				HighWatermarkPercent: ptr.To[int32](85),
				DownloadConcurrency:  ptr.To[int32](16),
				HTTPSProxy:           ptr.To("http://proxy.example:3128"),
			}},
			want: workercore.NodeModelStoreSpec{
				Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 85, LowPercent: 70},
				Download:   workercore.NodeModelStoreDownload{Concurrency: 16},
				Hub: workercore.NodeModelStoreHub{
					HuggingFaceEndpoint: "https://huggingface.co",
					HTTPSProxy:          "http://proxy.example:3128",
				},
			},
		},
		{
			name: "a set empty string overrides, where an unset field does not",
			layers: []Layer{
				func() Layer { l := clusterLayer(); l.NoProxy = ptr.To("internal"); return l }(),
				{NoProxy: ptr.To("")},
			},
			want: validSpec(),
		},
		{
			name:   "a later layer wins over an earlier one",
			layers: []Layer{clusterLayer(), {LowWatermarkPercent: ptr.To[int32](60)}, {LowWatermarkPercent: ptr.To[int32](50)}},
			want: func() workercore.NodeModelStoreSpec {
				s := validSpec()
				s.Watermarks.LowPercent = 50
				return s
			}(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, Merge(c.layers...))
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*workercore.NodeModelStoreSpec)
		wantErr string
	}{
		{name: "the defaults are valid", mutate: func(*workercore.NodeModelStoreSpec) {}},
		{
			name: "an http endpoint and a proxy without credentials are valid",
			mutate: func(s *workercore.NodeModelStoreSpec) {
				s.Hub.HuggingFaceEndpoint = "http://hub.local:8080"
				s.Hub.HTTPSProxy = "http://p:3128"
			},
		},
		{name: "low equal to high", mutate: func(s *workercore.NodeModelStoreSpec) { s.Watermarks.LowPercent = 80 }, wantErr: "watermarks"},
		{name: "low above high", mutate: func(s *workercore.NodeModelStoreSpec) { s.Watermarks.LowPercent = 90 }, wantErr: "watermarks"},
		{name: "high above 95", mutate: func(s *workercore.NodeModelStoreSpec) { s.Watermarks.HighPercent = 96 }, wantErr: "watermarks"},
		{name: "low of zero", mutate: func(s *workercore.NodeModelStoreSpec) { s.Watermarks.LowPercent = 0 }, wantErr: "watermarks"},
		{name: "no concurrency", mutate: func(s *workercore.NodeModelStoreSpec) { s.Download.Concurrency = 0 }, wantErr: "concurrency"},
		{name: "too much concurrency", mutate: func(s *workercore.NodeModelStoreSpec) { s.Download.Concurrency = 65 }, wantErr: "concurrency"},
		{name: "a negative bandwidth", mutate: func(s *workercore.NodeModelStoreSpec) { s.Download.BytesPerSecond = -1 }, wantErr: "bandwidth"},
		{name: "an endpoint without a scheme", mutate: func(s *workercore.NodeModelStoreSpec) { s.Hub.HuggingFaceEndpoint = "huggingface.co" }, wantErr: "endpoint"},
		{name: "an ftp endpoint", mutate: func(s *workercore.NodeModelStoreSpec) { s.Hub.HuggingFaceEndpoint = "ftp://hub" }, wantErr: "endpoint"},
		{name: "a blank endpoint", mutate: func(s *workercore.NodeModelStoreSpec) { s.Hub.HuggingFaceEndpoint = "" }, wantErr: "endpoint"},
		{name: "a proxy with credentials", mutate: func(s *workercore.NodeModelStoreSpec) { s.Hub.HTTPSProxy = "http://u:p@proxy:3128" }, wantErr: "proxy"},
		{
			name: "kubelet thresholds as percentages and quantities are valid",
			mutate: func(s *workercore.NodeModelStoreSpec) {
				s.Kubelet = &workercore.NodeModelStoreKubelet{NodefsAvailable: "10%", ImagefsAvailable: "20Gi", ImageGCHighThresholdPercent: ptr.To[int32](85)}
			},
		},
		{name: "kubelet thresholds all unset are valid", mutate: func(s *workercore.NodeModelStoreSpec) { s.Kubelet = &workercore.NodeModelStoreKubelet{} }},
		{
			name: "a nodefs threshold that is neither a percentage nor a quantity",
			mutate: func(s *workercore.NodeModelStoreSpec) {
				s.Kubelet = &workercore.NodeModelStoreKubelet{NodefsAvailable: "ten"}
			},
			wantErr: "nodefsAvailable",
		},
		{
			name: "an imagefs percentage above 100",
			mutate: func(s *workercore.NodeModelStoreSpec) {
				s.Kubelet = &workercore.NodeModelStoreKubelet{ImagefsAvailable: "150%"}
			},
			wantErr: "imagefsAvailable",
		},
		{
			name: "a negative quantity",
			mutate: func(s *workercore.NodeModelStoreSpec) {
				s.Kubelet = &workercore.NodeModelStoreKubelet{NodefsAvailable: "-1Gi"}
			},
			wantErr: "nodefsAvailable",
		},
		{
			name: "an image threshold above 100",
			mutate: func(s *workercore.NodeModelStoreSpec) {
				s.Kubelet = &workercore.NodeModelStoreKubelet{ImageGCHighThresholdPercent: ptr.To[int32](101)}
			},
			wantErr: "imageGCHighThresholdPercent",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSpec()
			c.mutate(&s)
			err := Validate(s)
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

func TestValidateReportsEveryRule(t *testing.T) {
	s := validSpec()
	s.Watermarks.LowPercent = 90
	s.Download.Concurrency = 0
	s.Hub.HuggingFaceEndpoint = ""

	err := Validate(s)
	require.Error(t, err)
	for _, want := range []string{"watermarks", "concurrency", "endpoint"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestParseBandwidth(t *testing.T) {
	cases := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{raw: "0", want: 0},
		{raw: "200Mi", want: 200 << 20},
		{raw: "1G", want: 1_000_000_000},
		{raw: "-1Mi", wantErr: true},
		{raw: "fast", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, err := ParseBandwidth(c.raw)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestParseThreshold(t *testing.T) {
	cases := []struct {
		raw         string
		wantPercent float64
		wantBytes   int64
		wantErr     bool
	}{
		{raw: "10%", wantPercent: 10},
		{raw: "7.5%", wantPercent: 7.5},
		{raw: "85%", wantPercent: 85},
		{raw: "0%", wantPercent: 0},
		{raw: "100%", wantPercent: 100},
		{raw: "20Gi", wantBytes: 20 << 30},
		{raw: "500M", wantBytes: 500_000_000},
		{raw: "101%", wantErr: true},
		{raw: "-1%", wantErr: true},
		{raw: "NaN%", wantErr: true},
		{raw: "+Inf%", wantErr: true},
		{raw: "-Inf%", wantErr: true},
		{raw: "-1Gi", wantErr: true},
		{raw: "ten", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, err := ParseThreshold(c.raw)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Threshold{Percent: c.wantPercent, Bytes: c.wantBytes, IsPercent: c.wantBytes == 0}, got)
		})
	}
}
