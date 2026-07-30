package virtualmachinebmc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
)

type podTemplate struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Spec        corev1.PodSpec    `json:"spec"`
}

func stampPodTemplateHash(pod *corev1.Pod) error {
	annotations := make(map[string]string, len(pod.Annotations))
	for key, value := range pod.Annotations {
		if key != PodTemplateHashAnnotation {
			annotations[key] = value
		}
	}

	data, err := json.Marshal(podTemplate{
		Labels:      pod.Labels,
		Annotations: annotations,
		Spec:        pod.Spec,
	})
	if err != nil {
		return err
	}

	sum := sha256.Sum256(data)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[PodTemplateHashAnnotation] = hex.EncodeToString(sum[:])
	return nil
}
