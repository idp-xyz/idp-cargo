package bootstrap

import (
	"context"
	"testing"
)

func TestOpenRequiresDSN(t *testing.T) {
	if _, err := Open(context.Background(), Config{}); err == nil {
		t.Fatal("Open without a DSN must fail")
	}
}
