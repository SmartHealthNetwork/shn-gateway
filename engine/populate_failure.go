package engine

import (
	"context"
	"errors"
)

// PopulateFailure describes a native population failure without payload data.
// Stage and Reason are closed categories, never upstream error text. Status is
// the observed population HTTP status (100..599), or 0 when none was observed;
// it is not the token endpoint's status or the caller's 502.
type PopulateFailure struct {
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
	Status int    `json:"status"`
}

const (
	populateStageRequestBuild       = "request_build"
	populateStageTokenAcquisition   = "token_acquisition"
	populateStageTransport          = "transport"
	populateStageBodyRead           = "body_read"
	populateStageHTTPStatus         = "http_status"
	populateStageQRExtract          = "qr_extract"
	populateReasonCanceled          = "canceled"
	populateReasonDeadline          = "deadline"
	populateReasonOther             = "other"
	populateReasonNon2xx            = "non_2xx"
	populateReasonInvalidJSON       = "invalid_json"
	populateReasonWrongResourceType = "wrong_resource_type"
)

func populateBoundaryFailure(stage string, status int, err error) *PopulateFailure {
	reason := populateReasonOther
	switch {
	case errors.Is(err, context.Canceled):
		reason = populateReasonCanceled
	case errors.Is(err, context.DeadlineExceeded):
		reason = populateReasonDeadline
	}
	return &PopulateFailure{Stage: stage, Reason: reason, Status: status}
}

func populateObservedStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func (n *nativePopulator) observeFailure(note PopulateFailure) {
	if n.observer != nil {
		n.observer(note)
	}
}
