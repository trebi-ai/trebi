package requests

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseSchemaFiles(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("testdata", "schema", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schema fixtures: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseSchema(raw)
		ok := filepath.Base(f)[:3] == "ok-"
		if ok != (err == nil) {
			t.Errorf("%s: err %v", f, err)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v is not ErrInvalid", f, err)
		}
	}
}

func TestValidateValues(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "schema", "ok-order.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); !slices.Equal(got, []string{"count", "express", "name", "notes", "size", "weight"}) {
		t.Errorf("names %v", got)
	}
	cases := []struct {
		values string
		field  string
	}{
		{`{"name":"Ana","count":2,"weight":1.5,"express":true,"size":"S","notes":"a\nb"}`, ""},
		{`{"name":"Ana"}`, ""},
		{`{"count":2}`, "values.name"},
		{`{"name":""}`, "values.name"},
		{`{"name":"Ana","count":"2"}`, "values.count"},
		{`{"name":"Ana","count":2.5}`, "values.count"},
		{`{"name":"Ana","weight":"heavy"}`, "values.weight"},
		{`{"name":"Ana","express":"yes"}`, "values.express"},
		{`{"name":"Ana","size":"XXL"}`, "values.size"},
		{`{"name":"Ana","color":"red"}`, "values.color"},
		{`[1]`, "values"},
	}
	for _, c := range cases {
		err := s.ValidateValues(json.RawMessage(c.values))
		var fe *FieldError
		switch {
		case c.field == "" && err != nil:
			t.Errorf("%s: %v", c.values, err)
		case c.field != "" && (!errors.As(err, &fe) || fe.Field != c.field):
			t.Errorf("%s: err %v, want field %s", c.values, err, c.field)
		}
	}
}
