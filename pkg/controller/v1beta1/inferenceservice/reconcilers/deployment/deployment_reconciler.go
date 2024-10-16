/*
Copyright 2021 The KServe Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"knative.dev/pkg/kmp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("DeploymentReconciler")

// DeploymentReconciler reconciles the raw kubernetes deployment resource
type DeploymentReconciler struct {
	client       kclient.Client
	scheme       *runtime.Scheme
	Deployment   *appsv1.Deployment
	componentExt *v1beta1.ComponentExtensionSpec
}

const (
	tlsVolumeName = "proxy-tls"
)

func NewDeploymentReconciler(client kclient.Client,
	scheme *runtime.Scheme,
	componentMeta metav1.ObjectMeta,
	componentExt *v1beta1.ComponentExtensionSpec,
	podSpec *corev1.PodSpec) *DeploymentReconciler {
	return &DeploymentReconciler{
		client:       client,
		scheme:       scheme,
		Deployment:   createRawDeployment(client, componentMeta, componentExt, podSpec),
		componentExt: componentExt,
	}
}

func createRawDeployment(cli kclient.Client, componentMeta metav1.ObjectMeta,
	componentExt *v1beta1.ComponentExtensionSpec, //nolint:unparam
	podSpec *corev1.PodSpec) *appsv1.Deployment {
	podMetadata := componentMeta
	podMetadata.Labels["app"] = constants.GetRawServiceLabel(componentMeta.Name)
	setDefaultPodSpec(podSpec)
	if val, ok := componentMeta.Labels[constants.ODHKserveRawAuth]; ok && val == "true" {
		kserveContainerPort := GetKServeContainerPort(podSpec)
		if kserveContainerPort == "" {
			kserveContainerPort = constants.InferenceServiceDefaultHttpPort
		}
		oauthProxyContainer, err := generateOauthProxyContainer(cli, kserveContainerPort, componentMeta.Namespace)
		if err != nil {
			podSpec.Containers = append(podSpec.Containers, oauthProxyContainer)
		}
		tlsSecretVolume := corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  componentMeta.Name,
					DefaultMode: func(i int32) *int32 { return &i }(420), // Directly use a pointer
				},
			},
		}
		podSpec.Volumes = append(podSpec.Volumes, tlsSecretVolume)
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: componentMeta,
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": constants.GetRawServiceLabel(componentMeta.Name),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: podMetadata,
				Spec:       *podSpec,
			},
		},
	}
	if componentExt.DeploymentStrategy != nil {
		deployment.Spec.Strategy = *componentExt.DeploymentStrategy
	}
	setDefaultDeploymentSpec(&deployment.Spec)
	return deployment
}

func GetKServeContainerPort(podSpec *corev1.PodSpec) string {
	for _, container := range podSpec.Containers {
		if container.Name == "kserve-container" {
			if len(container.Ports) > 0 {
				return strconv.Itoa(int(container.Ports[0].ContainerPort))
			}
		}
	}
	return ""
}

func generateOauthProxyContainer(cli kclient.Client, upstreamPort string, namespace string) (corev1.Container, error) {
	oauthImage := constants.OauthProxyImage
	oauthCpuLimit := constants.OauthProxyResourceCPULimit
	oauthMemoryLimit := constants.OauthProxyResourceMemoryLimit
	oauthCpuRequest := constants.OauthProxyResourceCPURequest
	oauthMemoryRequest := constants.OauthProxyResourceMemoryRequest
	inferenceServiceConfigMap := &corev1.ConfigMap{}
	err := cli.Get(context.TODO(), client.ObjectKey{
		Namespace: constants.KServeNamespace,
		Name:      constants.InferenceServiceConfigMapName,
	}, inferenceServiceConfigMap)
	if err == nil {
		var oauthData map[string]interface{}
		if err := json.Unmarshal([]byte(inferenceServiceConfigMap.Data["deploy"]), &oauthData); err != nil {
			return corev1.Container{}, fmt.Errorf("error retrieving value for key 'oauthProxy' from configmap %s. %w",
				constants.InferenceServiceConfigMapName, err)
		}
		if str, ok := oauthData["image"].(string); !ok {
			oauthImage = str
		}
		if str, ok := oauthData["cpuLimit"].(string); !ok {
			oauthCpuLimit = str
		}
		if str, ok := oauthData["memoryLimit"].(string); !ok {
			oauthMemoryLimit = str
		}
		if str, ok := oauthData["cpuRequest"].(string); !ok {
			oauthCpuRequest = str
		}
		if str, ok := oauthData["memoryRequest"].(string); !ok {
			oauthMemoryRequest = str
		}
	}

	return corev1.Container{
		Name: "oauth-proxy",
		Args: []string{
			`--https-address=:8443`,
			`--provider=openshift`,
			`--openshift-service-account=kserve-sa`,
			`--upstream=http://localhost:` + upstreamPort,
			`--tls-cert=/etc/tls/private/tls.crt`,
			`--tls-key=/etc/tls/private/tls.key`,
			`--cookie-secret=SECRET`,
			`--openshift-delegate-urls={"/": {"namespace": "` + namespace + `", "resource": "services", "verb": "get"}}`,
			`--openshift-sar={"namespace": "` + namespace + `", "resource": "services", "verb": "get"}`,
			`--skip-auth-regex="(^/metrics|^/apis/v1beta1/healthz)"`,
		},
		Image: oauthImage,
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: constants.OauthProxyPort,
				Name:          "https",
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromInt(constants.OauthProxyPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromInt(constants.OauthProxyPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 5,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(oauthCpuLimit),
				corev1.ResourceMemory: resource.MustParse(oauthMemoryLimit),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(oauthCpuRequest),
				corev1.ResourceMemory: resource.MustParse(oauthMemoryRequest),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      tlsVolumeName,
				MountPath: "/etc/tls/private",
			},
		},
	}, nil
}

// checkDeploymentExist checks if the deployment exists?
func (r *DeploymentReconciler) checkDeploymentExist(client kclient.Client) (constants.CheckResultType, *appsv1.Deployment, error) {
	// get deployment
	existingDeployment := &appsv1.Deployment{}
	err := client.Get(context.TODO(), types.NamespacedName{
		Namespace: r.Deployment.ObjectMeta.Namespace,
		Name:      r.Deployment.ObjectMeta.Name,
	}, existingDeployment)
	if err != nil {
		if apierr.IsNotFound(err) {
			return constants.CheckResultCreate, nil, nil
		}
		return constants.CheckResultUnknown, nil, err
	}
	// existed, check equivalence
	// for HPA scaling, we should ignore Replicas of Deployment
	ignoreFields := cmpopts.IgnoreFields(appsv1.DeploymentSpec{}, "Replicas")
	// Do a dry-run update. This will populate our local deployment object with any default values
	// that are present on the remote version.
	if err := client.Update(context.TODO(), r.Deployment, kclient.DryRunAll); err != nil {
		log.Error(err, "Failed to perform dry-run update of deployment", "Deployment", r.Deployment.Name)
		return constants.CheckResultUnknown, nil, err
	}
	if diff, err := kmp.SafeDiff(r.Deployment.Spec, existingDeployment.Spec, ignoreFields); err != nil {
		return constants.CheckResultUnknown, nil, err
	} else if diff != "" {
		log.Info("Deployment Updated", "Diff", diff)
		return constants.CheckResultUpdate, existingDeployment, nil
	}
	return constants.CheckResultExisted, existingDeployment, nil
}

func setDefaultPodSpec(podSpec *corev1.PodSpec) {
	if podSpec.DNSPolicy == "" {
		podSpec.DNSPolicy = corev1.DNSClusterFirst
	}
	if podSpec.RestartPolicy == "" {
		podSpec.RestartPolicy = corev1.RestartPolicyAlways
	}
	if podSpec.TerminationGracePeriodSeconds == nil {
		TerminationGracePeriodSeconds := int64(corev1.DefaultTerminationGracePeriodSeconds)
		podSpec.TerminationGracePeriodSeconds = &TerminationGracePeriodSeconds
	}
	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if podSpec.SchedulerName == "" {
		podSpec.SchedulerName = corev1.DefaultSchedulerName
	}
	for i := range podSpec.Containers {
		container := &podSpec.Containers[i]
		if container.TerminationMessagePath == "" {
			container.TerminationMessagePath = "/dev/termination-log"
		}
		if container.TerminationMessagePolicy == "" {
			container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
		if container.ImagePullPolicy == "" {
			container.ImagePullPolicy = corev1.PullIfNotPresent
		}
		// generate default readiness probe for model server container and for transformer container in case of collocation
		if container.Name == constants.InferenceServiceContainerName || container.Name == constants.TransformerContainerName {
			if container.ReadinessProbe == nil {
				if len(container.Ports) == 0 {
					container.ReadinessProbe = &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{
								Port: intstr.IntOrString{
									IntVal: 8080,
								},
							},
						},
						TimeoutSeconds:   1,
						PeriodSeconds:    10,
						SuccessThreshold: 1,
						FailureThreshold: 3,
					}
				} else {
					container.ReadinessProbe = &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{
								Port: intstr.IntOrString{
									IntVal: container.Ports[0].ContainerPort,
								},
							},
						},
						TimeoutSeconds:   1,
						PeriodSeconds:    10,
						SuccessThreshold: 1,
						FailureThreshold: 3,
					}
				}
			}
		}
	}
}

func setDefaultDeploymentSpec(spec *appsv1.DeploymentSpec) {
	if spec.Strategy.Type == "" {
		spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
	if spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && spec.Strategy.RollingUpdate == nil {
		spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
			MaxSurge:       &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
		}
	}
	if spec.RevisionHistoryLimit == nil {
		revisionHistoryLimit := int32(10)
		spec.RevisionHistoryLimit = &revisionHistoryLimit
	}
	if spec.ProgressDeadlineSeconds == nil {
		progressDeadlineSeconds := int32(600)
		spec.ProgressDeadlineSeconds = &progressDeadlineSeconds
	}

	spec.Template.Spec.ServiceAccountName = constants.KserveServiceAccountName
}

// Reconcile ...
func (r *DeploymentReconciler) Reconcile() (*appsv1.Deployment, error) {
	// Reconcile Deployment
	checkResult, deployment, err := r.checkDeploymentExist(r.client)
	if err != nil {
		return nil, err
	}
	log.Info("deployment reconcile", "checkResult", checkResult, "err", err)

	var opErr error
	switch checkResult {
	case constants.CheckResultCreate:
		opErr = r.client.Create(context.TODO(), r.Deployment)
	case constants.CheckResultUpdate:
		opErr = r.client.Update(context.TODO(), r.Deployment)
	default:
		return deployment, nil
	}

	if opErr != nil {
		return nil, opErr
	}

	return r.Deployment, nil
}
