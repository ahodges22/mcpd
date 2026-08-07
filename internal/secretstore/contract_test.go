package secretstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/internal/secretstore"
	"github.com/ahodges22/mcpd/internal/secretstore/secretstoretest"
)

func TestValidateValuePortableContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "ascii", value: "secret"},
		{name: "spaces", value: "  secret  "},
		{name: "punctuation", value: "quotes='\"' slash=\\ dollar=$ tick=`"},
		{name: "unicode", value: "café-雪-🔐"},
		{name: "portable limit", value: strings.Repeat("a", secretstore.MaxValueBytes)},
		{name: "empty", wantErr: true},
		{name: "invalid utf8", value: string([]byte{0xff}), wantErr: true},
		{name: "nul", value: "a\x00b", wantErr: true},
		{name: "c0", value: "a\nb", wantErr: true},
		{name: "del", value: "a\x7fb", wantErr: true},
		{name: "nonprinting unicode", value: "a\u200bb", wantErr: true},
		{name: "too large", value: strings.Repeat("a", secretstore.MaxValueBytes+1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := secretstore.ValidateValue(tc.value)
			if tc.wantErr && err == nil {
				t.Fatal("ValidateValue accepted invalid input")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateValue: %v", err)
			}
			if tc.wantErr {
				if condition, ok := secretstore.ConditionOf(err); !ok || condition != secretstore.ConditionInvalidValue {
					t.Fatalf("condition = %q, %v; want %q", condition, ok, secretstore.ConditionInvalidValue)
				}
			}
		})
	}
}

func TestProviderConditionsDoNotExposeValues(t *testing.T) {
	const value = "do-not-print-this-secret"
	err := &secretstore.Error{
		Operation: secretstore.OperationSet,
		Provider:  "native",
		Name:      "API_TOKEN",
		Condition: secretstore.ConditionDenied,
		Cause:     errors.New("provider rejected " + value),
	}

	if strings.Contains(err.Error(), value) {
		t.Fatalf("Error disclosed secret value: %s", err)
	}
	if !errors.Is(err, err.Cause) {
		t.Fatal("Error does not preserve its underlying category")
	}
	if condition, ok := secretstore.ConditionOf(err); !ok || condition != secretstore.ConditionDenied {
		t.Fatalf("condition = %q, %v; want %q", condition, ok, secretstore.ConditionDenied)
	}
	if got := (&secretstore.Error{}).Error(); got != string(secretstore.ConditionUnexpected) {
		t.Fatalf("empty typed error = %q, want %q", got, secretstore.ConditionUnexpected)
	}
}

func TestSetGetRoundTripsPrintableCorpus(t *testing.T) {
	ctx := context.Background()
	store := secretstoretest.NewMemory()
	var provider secretstore.Provider = store

	corpus := []string{
		"plain",
		"  leading and trailing  ",
		"quotes ' \" backslash \\ dollar $ backtick `",
		"multibyte café 雪 🔐",
	}
	for i, want := range corpus {
		name := "TOKEN_" + string(rune('A'+i))
		if err := provider.Set(ctx, name, want); err != nil {
			t.Fatalf("Set(%s): %v", name, err)
		}
		got, err := provider.Get(ctx, name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if !got.Present || got.Value != want {
			t.Fatalf("Get(%s) = %#v, want byte-exact %q", name, got, want)
		}
	}

	if err := provider.Delete(ctx, "TOKEN_A"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := provider.Get(ctx, "TOKEN_A")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got.Present || got.Value != "" {
		t.Fatalf("Get after Delete = %#v, want clean miss", got)
	}
}

func TestMemoryProviderNormalizesDeadlineCondition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := secretstoretest.NewMemory().Get(ctx, "TOKEN")
	if condition, ok := secretstore.ConditionOf(err); !ok || condition != secretstore.ConditionTimedOut {
		t.Fatalf("condition = %q, %v; want %q", condition, ok, secretstore.ConditionTimedOut)
	}
}
