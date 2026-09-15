package auth

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/telemetry"
)

// Measure counts what the guard decides with m: `cr.auth.logins` by the
// authenticator that accepted the credential and the outcome, and
// `cr.auth.denied` by action and by where it was refused -- at the token
// endpoint, or on a request. It answers g.
func (g *Guard) Measure(m metric.Meter) *Guard {
	g.logins = telemetry.Counter(m, "cr.auth.logins", "{login}", "Credentials checked, by the authenticator that accepted them and the outcome.")
	g.denied = telemetry.Counter(m, "cr.auth.denied", "{action}", "Actions refused, by action and by where.")
	return g
}

// login counts a credential checked: via is the authenticator that accepted
// it, and `none` when nobody did.
func (g *Guard) login(ctx context.Context, via, outcome string) {
	if g.logins == nil {
		return
	}
	if via == "" {
		via = "none"
	}
	g.logins.Add(ctx, 1, metric.WithAttributes(
		attribute.String("cr.auth.authenticator", via),
		attribute.String("cr.auth.outcome", outcome),
	))
}

// Denied counts actions refused at `at`: `token` when a token was issued
// without them, `request` when a request asked for them.
func (g *Guard) Denied(ctx context.Context, at string, actions []Action) {
	if g.denied == nil {
		return
	}
	for _, a := range actions {
		g.denied.Add(ctx, 1, metric.WithAttributes(
			attribute.String("cr.auth.action", string(a)),
			attribute.String("cr.auth.at", at),
		))
	}
}

// Measure reports the store with m: `cr.auth.policy.age`, the seconds since
// the bindings and tag rules were last loaded, and
// `cr.auth.policy.refresh.errors`, the loads that failed. A load that fails
// keeps the policy in force, and the age is how that shows. It answers st.
func (st *PolicyStore) Measure(m metric.Meter) *PolicyStore {
	st.refreshErrors = telemetry.Counter(m, "cr.auth.policy.refresh.errors", "{load}", "Loads of the bindings and tag rules that failed.")
	telemetry.Meter(m).Int64ObservableGauge("cr.auth.policy.age",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the bindings and tag rules were last loaded."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if last := st.last.Load(); last != 0 {
				o.Observe(int64(time.Since(time.Unix(0, last)).Seconds()))
			}
			return nil
		}),
	)
	return st
}
