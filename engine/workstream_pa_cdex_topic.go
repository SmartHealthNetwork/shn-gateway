//go:build !shn_coreonly

package engine

import (
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The HRex Task topic's legs (pa/cdextopic): the subscribe leg the provider's
// gateway serves, and the signal leg every CDex notification rides, served by
// a later release.
const (
	legCDexTaskSubscribe = cdextopic.SubscribeLeg
	legCDexTaskSignal    = cdextopic.SignalLeg
)

// payerHolder is the authenticated payer as the registry attests it, for the
// topic's filter rule: its participant id and its attested payer identifiers.
func payerHolder(payer string, ids []shnsdk.PayerIdentifier) subscription.Holder {
	h := subscription.Holder{ID: payer}
	for _, id := range ids {
		h.PayerIDs = append(h.PayerIDs, subscription.Identifier{System: id.System, Value: id.Value})
	}
	return h
}
