package main

import (
	"strings"

	"github.com/poteto/noodle/internal/admission"
)

func (host *noodleHost) historicalHostDeferral(request hostRequest) (hostResponse, bool) {
	if request.Operation != "status" || request.RequestID == "" || request.RequiredHostInstanceID == "" {
		return hostResponse{}, false
	}
	for _, receipt := range host.receipts {
		known := receipt.AdmissionRefusal == admission.ErrDeferred.Error() || strings.HasPrefix(receipt.AdmissionRefusal, admission.ErrDeferred.Error()+": ")
		if known && receipt.ContextState == "detached" && receipt.Mode == "manual" &&
			receipt.HostInstanceID == request.RequiredHostInstanceID && receipt.BinarySHA256 == request.BinarySHA256 &&
			host.contexts[receipt.ContextID] == nil && hostReceiptMatches(request, receipt) {
			return hostReceiptResult(receipt), true
		}
	}
	return hostResponse{}, false
}
