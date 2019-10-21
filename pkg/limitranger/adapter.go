package limitranger

import (
	admissionlimitranger "k8s.io/kubernetes/plugin/pkg/admission/limitranger"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
)

func NewAdapter(limitRanger *admissionlimitranger.LimitRanger) *Adapter {
	return &Adapter{
		limitRanger: limitRanger,
	}
}

type Adapter struct {
	limitRanger *admissionlimitranger.LimitRanger
}

func (a *Adapter) Admit(admissionSpec *admissionv1beta1.AdmissionRequest) error {
	// TODO: Figure out whether we need to invoke LimitRanger.Admit

	a.limitRanger.Admit()

	return nil
}
