package crd_gen_alias_marker_negative

// Target is the CRD type of this fixture, and it carries the comment tag on
// itself.
//
// This is the negative control of the alias comment tag fixture: the same
// comment tag, on the type the CRD is generated from, does configure a printer
// column. The tag of an alias configures nothing, so the column the alias
// fixture does not produce is a real distinction and not a marker that never
// reaches anything.
//
// +k8s:crd-gen:resource:scope="Namespaced",plural="targets"
// +k8s:crd-gen:printcolumn:name="MarkerAlias",type="string",jsonPath=".spec.markerAlias.field"
type Target struct {
	Name string `json:"name,omitempty"`
}
