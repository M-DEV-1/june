package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

// decodeShapesForTest unmarshals each of the given JSON object literals into a map, the same way shapeStream.Push does its own emitted objects, so a test's expected shapes are built by the same rules as what it compares them against.
func decodeShapesForTest(t *testing.T, objs []string) []map[string]any {
	t.Helper()
	if objs == nil {
		return nil
	}
	want := make([]map[string]any, len(objs))
	for i, raw := range objs {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("test data %d (%q): %v", i, raw, err)
		}
		want[i] = m
	}
	return want
}

// TestShapeStream_EmitsShapesAsTheyClose feeds the same call to a shapeStream two ways — whole, and split into single-byte fragments — and checks both produce the same shapes, in the same order, as unmarshaling the expected object literals directly.
func TestShapeStream_EmitsShapesAsTheyClose(t *testing.T) {
	cases := []struct {
		name string
		call string
		want []string
	}{
		{
			name: "two shapes, one with a nested rect and one with nested points",
			call: `{"shapes":[{"shape":"box","rect":{"x":180,"y":277,"w":375,"h":540},"label":"ENCODER xN"},{"shape":"arrow","points":[[360,830],[360,820]],"label":"in"}]}`,
			want: []string{
				`{"shape":"box","rect":{"x":180,"y":277,"w":375,"h":540},"label":"ENCODER xN"}`,
				`{"shape":"arrow","points":[[360,830],[360,820]],"label":"in"}`,
			},
		},
		{
			name: "a nested rect object is not emitted on its own",
			call: `{"shapes":[{"shape":"box","rect":{"x":1,"y":2,"w":3,"h":4}}]}`,
			want: []string{`{"shape":"box","rect":{"x":1,"y":2,"w":3,"h":4}}`},
		},
		{
			name: "a brace inside a string label is not read as structure",
			call: `{"shapes":[{"shape":"box","label":"a } brace"}]}`,
			want: []string{`{"shape":"box","label":"a } brace"}`},
		},
		{
			name: "an escaped quote and an escaped backslash inside a label",
			call: `{"shapes":[{"shape":"box","label":"a \"quoted\" \\ word"}]}`,
			want: []string{`{"shape":"box","label":"a \"quoted\" \\ word"}`},
		},
		{
			name: "no shapes key emits nothing",
			call: `{"shape":"box","rect":{"x":1,"y":2,"w":3,"h":4}}`,
			want: nil,
		},
		{
			name: "a top-level shapes that is not an array emits nothing",
			call: `{"shapes":{"shape":"box","on":1}}`,
			want: nil,
		},
		{
			name: "a second top-level array after shapes closes draws nothing extra",
			call: `{"shapes":[{"shape":"box","on":1}],"extra":[{"shape":"circle","on":2}]}`,
			want: []string{`{"shape":"box","on":1}`},
		},
		{
			name: "a shape carrying its own shapes key is still captured whole",
			call: `{"shapes":[{"shape":"box","shapes":"nested","on":1}]}`,
			want: []string{`{"shape":"box","shapes":"nested","on":1}`},
		},
		{
			name: "unbalanced input still emits the shape whose own braces closed",
			call: `{"shapes":[{"shape":"box"}`,
			want: []string{`{"shape":"box"}`},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := decodeShapesForTest(t, c.want)

			var whole shapeStream
			assertShapesEqual(t, "whole call in one Push", whole.Push(c.call), want)

			var bytewise shapeStream
			var gotSplit []map[string]any
			for i := 0; i < len(c.call); i++ {
				gotSplit = append(gotSplit, bytewise.Push(c.call[i:i+1])...)
			}
			assertShapesEqual(t, "the same call split into single-byte fragments", gotSplit, want)
		})
	}
}

// assertShapesEqual fails the test when got and want do not carry the same shapes in the same order. Input: a label naming which of the two Push modes produced got, plus the two shape lists. Output: none — it reports through t.
func assertShapesEqual(t *testing.T, mode string, got, want []map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d shapes, want %d: got=%v want=%v", mode, len(got), len(want), got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s: shape %d = %v, want %v", mode, i, got[i], want[i])
		}
	}
}
