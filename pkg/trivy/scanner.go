package trivy

import (
	"fmt"
	"io"

	"github.com/aquasecurity/starboard/pkg/find/vulnerabilities/trivy"

	"github.com/aquasecurity/starboard/pkg/apis/aquasecurity/v1alpha1"
	"github.com/aquasecurity/starboard/pkg/scanners"

	"github.com/aquasecurity/starboard-operator/pkg/scanner"
	"github.com/aquasecurity/starboard/pkg/kube"
	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

const (
	trivyImageRef = "aquasec/trivy:0.11.0"
)

func NewScanner() scanner.VulnerabilityScanner {
	return &trivyScanner{}
}

type trivyScanner struct {
}

func (s *trivyScanner) NewScanJob(workload kube.Object, spec corev1.PodSpec, options scanner.Options) (*batchv1.Job, *corev1.Secret, error) {
	jobName := fmt.Sprintf(uuid.New().String())

	initContainerName := jobName
	imagePullSecretName := jobName
	imagePullSecretData := make(map[string][]byte)
	var imagePullSecret *corev1.Secret

	initContainers := []corev1.Container{
		{
			Name:                     initContainerName,
			Image:                    trivyImageRef,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Command: []string{
				"trivy",
			},
			Args: []string{
				"--download-db-only",
				"--cache-dir",
				"/var/lib/trivy",
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "data",
					ReadOnly:  false,
					MountPath: "/var/lib/trivy",
				},
			},
		},
	}

	containerImages := kube.ContainerImages{}

	scanJobContainers := make([]corev1.Container, len(spec.Containers))
	for i, c := range spec.Containers {
		containerImages[c.Name] = c.Image

		var envs []corev1.EnvVar

		if dockerConfig, ok := options.ImageCredentials[c.Image]; ok {
			registryUsernameKey := fmt.Sprintf("%s.username", c.Name)
			registryPasswordKey := fmt.Sprintf("%s.password", c.Name)

			imagePullSecretData[registryUsernameKey] = []byte(dockerConfig.Username)
			imagePullSecretData[registryPasswordKey] = []byte(dockerConfig.Password)

			envs = append(envs, corev1.EnvVar{
				Name: "TRIVY_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: imagePullSecretName,
						},
						Key: registryUsernameKey,
					},
				},
			}, corev1.EnvVar{
				Name: "TRIVY_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: imagePullSecretName,
						},
						Key: registryPasswordKey,
					},
				},
			})
		}

		scanJobContainers[i] = corev1.Container{
			Name:                     c.Name,
			Image:                    trivyImageRef,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Env:                      envs,
			Command: []string{
				"trivy",
			},
			Args: []string{
				"--skip-update",
				"--cache-dir",
				"/var/lib/trivy",
				"--no-progress",
				"--format",
				"json",
				c.Image,
			},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("500M"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("100M"),
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "data",
					ReadOnly:  false,
					MountPath: "/var/lib/trivy",
				},
			},
		}
	}

	containerImagesAsJSON, err := containerImages.AsJSON()
	if err != nil {
		return nil, nil, err
	}

	if len(imagePullSecretData) > 0 {
		imagePullSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      imagePullSecretName,
				Namespace: options.Namespace,
				Labels: map[string]string{
					kube.LabelResourceKind:      string(workload.Kind),
					kube.LabelResourceName:      workload.Name,
					kube.LabelResourceNamespace: workload.Namespace,
				},
			},
			Data: imagePullSecretData,
		}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: options.Namespace,
			Labels: map[string]string{
				kube.LabelResourceKind:      string(workload.Kind),
				kube.LabelResourceName:      workload.Name,
				kube.LabelResourceNamespace: workload.Namespace,
			},
			Annotations: map[string]string{
				kube.AnnotationContainerImages: containerImagesAsJSON,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          pointer.Int32Ptr(0),
			Completions:           pointer.Int32Ptr(1),
			ActiveDeadlineSeconds: scanners.GetActiveDeadlineSeconds(options.ScanJobTimeout),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						kube.LabelResourceKind:      string(workload.Kind),
						kube.LabelResourceName:      workload.Name,
						kube.LabelResourceNamespace: workload.Namespace,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           options.ServiceAccountName,
					AutomountServiceAccountToken: pointer.BoolPtr(false),
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									Medium: corev1.StorageMediumDefault,
								},
							},
						},
					},
					InitContainers: initContainers,
					Containers:     scanJobContainers,
				},
			},
		},
	}, imagePullSecret, nil
}

func (s *trivyScanner) ParseVulnerabilityReport(imageRef string, logsReader io.ReadCloser) (v1alpha1.VulnerabilityReport, error) {
	return trivy.DefaultConverter.Convert(imageRef, logsReader)
}
