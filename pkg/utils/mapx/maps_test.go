package mapx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMerge(t *testing.T) {
	cases := []struct {
		name string
		ms   []map[string]string
		want map[string]string
	}{
		{
			name: "no maps",
			ms:   nil,
			want: nil,
		},
		{
			name: "all nil or empty maps",
			ms:   []map[string]string{nil, {}},
			want: nil,
		},
		{
			name: "disjoint keys combine",
			ms:   []map[string]string{{"foo": "1"}, {"bar": "2"}},
			want: map[string]string{"foo": "1", "bar": "2"},
		},
		{
			name: "later map wins on conflict",
			ms:   []map[string]string{{"foo": "1", "bar": "2"}, {"foo": "9"}},
			want: map[string]string{"foo": "9", "bar": "2"},
		},
		{
			name: "skips nil among non-nil",
			ms:   []map[string]string{nil, {"foo": "1"}, nil},
			want: map[string]string{"foo": "1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, Merge(c.ms...))
		})
	}
}

func TestEqualWithKey(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]string
		k    string
		ks   []string
		want bool
	}{
		{
			name: "equal on single key",
			a:    map[string]string{"foo": "1", "bar": "2"},
			b:    map[string]string{"foo": "1", "baz": "9"},
			k:    "foo",
			want: true,
		},
		{
			name: "value differs on single key",
			a:    map[string]string{"foo": "1"},
			b:    map[string]string{"foo": "2"},
			k:    "foo",
			want: false,
		},
		{
			name: "key only in a",
			a:    map[string]string{"foo": "1"},
			b:    map[string]string{"bar": "1"},
			k:    "foo",
			want: false,
		},
		{
			name: "key only in b",
			a:    map[string]string{"bar": "1"},
			b:    map[string]string{"foo": "1"},
			k:    "foo",
			want: false,
		},
		{
			name: "key absent in both non-empty maps",
			a:    map[string]string{"bar": "1"},
			b:    map[string]string{"baz": "1"},
			k:    "foo",
			want: true,
		},
		{
			name: "empty maps",
			a:    nil,
			b:    nil,
			k:    "foo",
			want: true,
		},
		{
			name: "a empty, b has key",
			a:    nil,
			b:    map[string]string{"foo": "1"},
			k:    "foo",
			want: false,
		},
		{
			name: "a empty, b without key",
			a:    nil,
			b:    map[string]string{"bar": "1"},
			k:    "foo",
			want: true,
		},
		{
			name: "equal across multiple keys",
			a:    map[string]string{"foo": "1", "bar": "2", "baz": "3"},
			b:    map[string]string{"foo": "1", "bar": "2", "qux": "9"},
			k:    "foo",
			ks:   []string{"bar"},
			want: true,
		},
		{
			name: "value differs on secondary key",
			a:    map[string]string{"foo": "1", "bar": "2"},
			b:    map[string]string{"foo": "1", "bar": "9"},
			k:    "foo",
			ks:   []string{"bar"},
			want: false,
		},
		{
			name: "secondary key missing in b",
			a:    map[string]string{"foo": "1", "bar": "2"},
			b:    map[string]string{"foo": "1"},
			k:    "foo",
			ks:   []string{"bar"},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EqualWithKey(c.a, c.b, c.k, c.ks...)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestEqualWithStringPrefix(t *testing.T) {
	cases := []struct {
		name     string
		a, b     map[string]string
		prefix   string
		prefixes []string
		want     bool
	}{
		{
			name:   "equal with single prefix",
			a:      map[string]string{"foo": "1", "bar": "3"},
			b:      map[string]string{"foo": "2", "bar": "3"},
			prefix: "ba",
			want:   true,
		},
		{
			name:     "equal across multiple prefixes",
			a:        map[string]string{"foo.x": "1", "bar.y": "2", "baz.z": "3", "qux": "0"},
			b:        map[string]string{"foo.x": "1", "bar.y": "2", "baz.z": "3", "qux": "9"},
			prefix:   "foo",
			prefixes: []string{"bar", "baz"},
			want:     true,
		},
		{
			name:     "value differs on secondary prefix",
			a:        map[string]string{"foo.x": "1", "bar.y": "2"},
			b:        map[string]string{"foo.x": "1", "bar.y": "9"},
			prefix:   "foo",
			prefixes: []string{"bar"},
			want:     false,
		},
		{
			name:     "secondary prefixed key only in b",
			a:        map[string]string{"foo.x": "1"},
			b:        map[string]string{"foo.x": "1", "bar.y": "2"},
			prefix:   "foo",
			prefixes: []string{"bar"},
			want:     false,
		},
		{
			name:     "secondary prefixed key only in a",
			a:        map[string]string{"foo.x": "1", "bar.y": "2"},
			b:        map[string]string{"foo.x": "1"},
			prefix:   "foo",
			prefixes: []string{"bar"},
			want:     false,
		},
		{
			name:     "no key matches any prefix",
			a:        map[string]string{"qux": "1"},
			b:        map[string]string{"qux": "2"},
			prefix:   "foo",
			prefixes: []string{"bar"},
			want:     true,
		},
		{
			name:     "empty maps",
			a:        nil,
			b:        nil,
			prefix:   "foo",
			prefixes: []string{"bar"},
			want:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EqualWithStringPrefix(c.a, c.b, c.prefix, c.prefixes...)
			assert.Equal(t, c.want, got)
		})
	}
}
