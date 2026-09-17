package fhirseed

import (
	"context"
	"fmt"
	"net/http"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// WarmDeadline bounds the seeder's first $validate. A cold HAPI pays its
// validation-support initialisation on the first $validate after boot — on a
// 1 vCPU task that first call has been measured near, and once past, the 30 s
// client budget every consumer gives a $validate — so the seeder pays it here,
// before the seed-complete marker the smokes wait on, with a deadline of its own
// that is deliberately far above the consumers' budget and deliberately far
// below the marker waiters' patience (the cloud smoke waits 900 s for the
// marker; a test in tools/cloudsmoke pins WarmDeadline*3 <= that wait).
const WarmDeadline = 300 * time.Second

// warmBody is the smallest resource that exercises the profile path consumers
// take: a US Core Patient declaring its profile.
const warmBody = `{"resourceType":"Patient","id":"seed-warm","meta":{"profile":["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"]},"identifier":[{"system":"urn:shn:seed","value":"warm"}],"name":[{"family":"Warm","given":["Seed"]}],"gender":"unknown"}`

// WarmValidate posts one $validate to {Base}/{tenant} through a client whose
// timeout is WarmDeadline — never c.HTTP, whose timeout is a consumer's — and
// returns how long the server took. The verdict is not the point (a cold
// server's answer is the same as a warm one's); the elapsed time is what the
// seeder logs, and an answer past the deadline is an error naming it.
func (c *Client) WarmValidate(ctx context.Context, tenant string) (time.Duration, error) {
	return c.warmValidate(ctx, tenant, WarmDeadline)
}

func (c *Client) warmValidate(ctx context.Context, tenant string, deadline time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	v := shnsdk.NewOperationValidator(c.Base + "/" + tenant)
	v.Client = &http.Client{Timeout: deadline}
	start := time.Now()
	if _, err := v.Validate(ctx, []byte(warmBody), ""); err != nil {
		if ctx.Err() != nil {
			return time.Since(start), fmt.Errorf("fhirseed: validator warm-up did not answer within %s: %w", deadline, err)
		}
		return time.Since(start), fmt.Errorf("fhirseed: validator warm-up: %w", err)
	}
	elapsed := time.Since(start)
	c.logf("fhirseed: validator warm in %.1fs (%s/%s)", elapsed.Seconds(), c.Base, tenant)
	return elapsed, nil
}
