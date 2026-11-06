// Package bento adapts IDP-Bento-Go to Cargo. It is shared by all bounded contexts and must not
// depend on any of them.
package bento

import (
	"context"
	"time"

	"go.idp.xyz/idp-bento-go/application"
)

var (
	_ application.Clock      = SystemClock{}
	_ application.Transactor = (*TransactionPort)(nil)
)

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// TransactionPort exposes a function as application.Transactor. TenantTransactor's callback type is
// unnamed, so it does not satisfy application.Transactor directly.
type TransactionPort struct {
	Within func(context.Context, application.TxFunc) error
}

func (p *TransactionPort) WithinTransaction(ctx context.Context, fn application.TxFunc) error {
	return p.Within(ctx, fn)
}
