package crd_gen

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/api/resource"
	core "k8s.io/api/core/v1"
)

// +k8s:crd-gen:printcolumn:name="Detached",type="string",jsonPath=".spec.detachedString"

// Dummy is the schema for the projects API.
//
// The print column above sits in a comment block of its own, separated from
// the doc comment by an empty line. gengo keeps such a block as the second
// closest comment of the type, and the generator reads the markers of both
// comment blocks, so a marker that sits there has to reach the schema just
// like one that sits inside the doc comment.
//
// The print columns are here for what they carry rather than for themselves. Priority is an int32
// and has to be emitted BARE: quoted, it produced a string constant in an int32 field and the
// generated file did not compile — which no test could catch while no type in this repository used
// the marker. One column leaves priority at its default and one sets it, so the expected output pins
// both forms.
//
// The third pins a jsonPath FILTER, whose value contains "=" twice. The marker parser splits on "=",
// so the filter has to survive being a value that looks like more key/value pairs — it once came
// back split across two junk entries as well as its own, which nothing noticed because unknown keys
// were dropped in silence.
//
// The fourth pins the "date" TYPE. It is the type kubectl's default Age column uses, and it was
// missing from the generator's accepted set: every date column was dropped, and dropped QUIETLY —
// the unknown type was logged and the column skipped, so `make generate` still succeeded and no
// golden file moved. Nothing here would notice it being dropped again without a date column to pin.
//
// +k8s:crd-gen:resource:categories=["all","walrus"],scope="Namespaced",shortName=["proj"],plural="projects",subResources=["status"]
// +k8s:crd-gen:printcolumn:name="String",type="string",jsonPath=".spec.string"
// +k8s:crd-gen:printcolumn:name="Integer",type="integer",jsonPath=".spec.integer",description="an integer",format="int32",priority=1
// +k8s:crd-gen:printcolumn:name="Filtered",type="string",jsonPath=".status.conditions[?(@.type=='Ready')].status"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type Dummy struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty"`

	Spec   DummySpec   `json:"spec,omitempty"`
	Status DummyStatus `json:"status,omitempty"`
}

// DummySpec defines the desired state of Dummy.
type DummySpec struct {
	// Primitive.

	// +optional
	// +k8s:validation:minimum=1
	Integer int `json:"integer,omitempty"`
	// +optional
	Float float64 `json:"float,omitempty"`
	// +optional
	// +k8s:validation:enum=["x","y","z"]
	String string `json:"string,omitempty"`
	// +optional
	Bool bool `json:"bool,omitempty"`
	// +k8s:validation:enum=[1,2,3,5,7]
	// +nullable=false
	IntegerPointer *int64 `json:"integerPointer,omitempty"`
	// +k8s:validation:maximum=10
	FloatPointer *float32 `json:"floatPointer,omitempty"`
	// +k8s:validation:default="x"
	StringPointer *string `json:"stringPointer,omitempty"`
	// +default=true
	BoolPointer *bool `json:"boolPointer,omitempty"`

	// WellKnown Extension.

	MicroTime           meta.MicroTime        `json:"microTime,omitempty"`
	Duration            meta.Duration         `json:"duration,omitempty"`
	Time                meta.Time             `json:"time,omitempty"`
	IntOrString         intstr.IntOrString    `json:"intOrString,omitempty"`
	RawExtension        runtime.RawExtension  `json:"rawExtension,omitempty"`
	MicroTimePointer    *meta.MicroTime       `json:"microTimePointer,omitempty"`
	DurationPointer     *meta.Duration        `json:"durationPointer,omitempty"`
	TimePointer         *meta.Time            `json:"timePointer,omitempty"`
	IntOrStringPointer  *intstr.IntOrString   `json:"intOrStringPointer,omitempty"`
	RawExtensionPointer *runtime.RawExtension `json:"rawExtensionPointer,omitempty"`

	// Map.

	// +mapType=atomic
	// +k8s:validation:maxProperties=3
	MapString      map[string]string  `json:"mapString,omitempty"`
	MapPointString map[string]*string `json:"mapPointString,omitempty"`
	MapObject      map[string]struct {
		A string              `json:"a"`
		B int                 `json:"b"`
		C bool                `json:"c"`
		D float64             `json:"d"`
		E map[string]struct{} `json:"e"`
		F []struct{}          `json:"f"`
	} `json:"mapObject,omitempty"`
	// +mapType=atomic
	MapInOrString    map[string]intstr.IntOrString `json:"mapInOrString,omitempty"`
	MapStringPointer *map[string]string            `json:"mapStringPointer,omitempty"`
	MapInterface     map[string]any                `json:"mapInterface,omitempty"`

	// Slice.

	// +k8s:validation:maxItems=3
	// +k8s:validation:minItems=1
	SliceString      []string  `json:"sliceString,omitempty"`
	SlicePointString []*string `json:"slicePointString,omitempty"`
	// +k8s:validation:cel[0]:rule="self.b > 0"
	// +k8s:validation:cel[0]:message="b must be greater than 0"
	SliceObject []struct {
		A string              `json:"a"`
		B int                 `json:"b"`
		C bool                `json:"c"`
		D float64             `json:"d"`
		E map[string]struct{} `json:"e"`
		F []struct{}          `json:"f"`
	} `json:"sliceObject,omitempty"`
	SliceBytes        []byte  `json:"sliceBytes,omitempty"`
	SliceBytesPointer *[]byte `json:"sliceBytesPointer,omitempty"`
	SliceInterface    []any   `json:"sliceInterface,omitempty"`

	// Array.

	ArrayString      [3]string  `json:"arrayString"`
	ArrayPointString [3]*string `json:"arrayPointString"`
	// +k8s:validation:cel[0]:rule> self.b > 0
	// +k8s:validation:cel[0]:message> b must be greater than 0,
	// +k8s:validation:cel[1]:rule> self.e.length() % 2 == 0
	// +k8s:validation:cel[1]:rule>   ? self.a == self.a + ' is even'
	// +k8s:validation:cel[1]:rule>   : self.a == self.a + ' is odd'
	// +k8s:validation:cel[1]:message> mutate a
	// +k8s:validation:cel[1]:reason="FieldValueRequired"
	ArrayObject [3]struct {
		A string              `json:"a"`
		B int                 `json:"b"`
		C bool                `json:"c"`
		D float64             `json:"d"`
		E map[string]struct{} `json:"e"`
	}

	// Reference.

	SubDummySpecPointer *DummySpec  `json:"subDummySpecPointer,omitempty"`
	SliceSubDummySpec   []DummySpec `json:"sliceSubDummySpec,omitempty"`
	DummyX              DummyX      `json:"dummyX"`
	DummyY              DummyY      `json:"dummyY,omitempty"`

	// Alias.
	//
	// A type alias has to resolve to the type it aliases: an alias of a named
	// struct has the schema of that struct, and an alias of a byte slice has
	// the schema of a byte slice. A defined type over an alias resolves the
	// same way. An alias that did not resolve would show up as an object
	// preserving unknown fields, and no field of this repository uses one, so
	// nothing would notice it without these fields.
	AliasTargetAlias  AliasTargetAlias  `json:"aliasTargetAlias,omitempty"`
	AliasBytes        AliasBytes        `json:"aliasBytes,omitempty"`
	DefinedAliasBytes DefinedAliasBytes `json:"definedAliasBytes,omitempty"`
	MarkerAlias       MarkerAlias       `json:"markerAlias,omitempty"`

	Quantity  resource.Quantity `json:"quantity"`
	Container core.Container    `json:"container"`
}

type AliasTarget struct {
	Field string `json:"field"`
}

// AliasTargetAlias is an alias of a named struct, so its schema is the schema
// of that struct.
type AliasTargetAlias = AliasTarget

// AliasBytes is an alias of a byte slice, so its schema is the schema of a
// byte slice.
type AliasBytes = []byte

// DefinedAliasBytes is a defined type over an alias of a byte slice, so its
// schema is the schema of a byte slice too.
type DefinedAliasBytes AliasBytes

type MarkerAliasTarget struct {
	Field string `json:"field"`
}

// MarkerAlias is an alias of a named struct that carries a comment tag of its
// own.
//
// The tag is the point of this alias. A comment tag on an alias configures
// nothing by itself: the CRD takes its tags from the CRD type, and the type
// this alias resolves to is not the CRD type, so the tag must not become a
// printer column of the CRD. The alias still has to resolve to the schema of
// the type it aliases.
//
// The type this alias resolves to carries no comment of its own, so gengo
// keeps the comment of the alias as the comment of that type. The tag is
// therefore within reach of the generator, and it is the generator that has to
// leave it alone.
//
// +k8s:crd-gen:printcolumn:name="MarkerAlias",type="string",jsonPath=".spec.markerAlias.field"
type MarkerAlias = MarkerAliasTarget

type DummyX struct {
	Z DummyZRef `json:"z"`
	X *DummyX   `json:"x"`
}

type DummyY struct {
	Z DummyZRef `json:"z"`
	Y *DummyY   `json:"y"`
}

type DummyZRef struct {
	Test string `json:"test"`
}

// DummyStatus defines the observed state of Dummy.
type DummyStatus struct {
	Conditions []meta.Condition `json:"conditions,omitempty"`
}
