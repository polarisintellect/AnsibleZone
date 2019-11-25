// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"fmt"
	"time"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	"k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // to avoid 'No Auth Provider found for name "gcp"'
	"k8s.io/client-go/tools/clientcmd"

	"istio.io/istio/pkg/log"
)

var (
	immediate int64
)

// CreateClientset creates a new Clientset for the given kubeconfig.
func CreateClientset(kubeconfig string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create config object from kube-config file: %q (error: %v)",
			kubeconfig, err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset object (error: %v)", err)
	}

	return clientset, nil
}

// CreateTestNamespace creates a namespace for test. Returns name of the namespace on success, and error if there is any.
func CreateTestNamespace(clientset kubernetes.Interface, prefix string) (string, error) {
	template := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix,
		},
	}
	namespace, err := clientset.CoreV1().Namespaces().Create(template)
	if err != nil {
		return "", fmt.Errorf("failed to create a namespace (error: %v)", err)
	}

	name := namespace.GetName()
	log.Infof("Namespace %v is created", name)
	return name, nil
}

// DeleteTestNamespace deletes a namespace for test.
func DeleteTestNamespace(clientset kubernetes.Interface, namespace string) error {
	if err := clientset.CoreV1().Namespaces().Delete(namespace, &metav1.DeleteOptions{GracePeriodSeconds: &immediate}); err != nil {
		return fmt.Errorf("failed to delete namespace %q (error: %v)", namespace, err)
	}
	log.Infof("Namespace %v is deleted", namespace)
	return nil
}

// CreateService creates a service object and returns a pointer pointing to this object on success.
func CreateService(clientset kubernetes.Interface, namespace string, name string, port int32,
	serviceType v1.ServiceType, pod *v1.Pod) (*v1.Service, error) {
	uuid := string(uuid.NewUUID())
	_, err := clientset.CoreV1().Services(namespace).Create(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"uuid": uuid,
			},
			Name: name,
		},
		Spec: v1.ServiceSpec{
			Type:     serviceType,
			Selector: pod.Labels,
			Ports: []v1.ServicePort{
				{
					Port: port,
				},
			},
		},
	})

	if err != nil {
		return nil, err
	}

	if serviceType == v1.ServiceTypeLoadBalancer {
		err = waitForServiceExternalIPAddress(clientset, namespace, uuid, 300*time.Second)
		if err != nil {
			return nil, err
		}
	}

	return clientset.CoreV1().Services(namespace).Get(name, metav1.GetOptions{})
}

// DeleteService deletes a service.
func DeleteService(clientset kubernetes.Interface, namespace string, name string) error {
	return clientset.CoreV1().Services(namespace).Delete(name, &metav1.DeleteOptions{GracePeriodSeconds: &immediate})
}

// CreatePod creates a pod object and returns a pointer pointing to this object on success.
func CreatePod(clientset kubernetes.Interface, namespace string, image string, name string) (*v1.Pod, error) {
	return CreatePodWithCommand(clientset, namespace, image, name, []string{}, []string{})
}

// CreatePodWithCommand creates a pod object with specific command and arguments and returns a pointer pointing to this object on success.
func CreatePodWithCommand(clientset kubernetes.Interface, namespace string, image string, name string, command []string, args []string) (*v1.Pod, error) {
	podUUID := string(uuid.NewUUID())

	env := []v1.EnvVar{
		{
			Name: "NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		},
	}

	spec := v1.PodSpec{
		Containers: []v1.Container{
			{
				Env:   env,
				Name:  fmt.Sprintf("%v-pod-container", name),
				Image: image,
			},
		},
	}

	if len(command) > 0 {
		spec.Containers[0].Command = command
		if len(args) > 0 {
			spec.Containers[0].Args = args
		}
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"uuid":      podUUID,
				"pod-group": fmt.Sprintf("%v-pod-group", name),
			},
			Name: name,
		},
		Spec: spec,
	}

	_, err := clientset.CoreV1().Pods(namespace).Create(pod)
	if err != nil {
		return nil, err
	}

	if err := waitForPodRunning(clientset, namespace, podUUID, 60*time.Second); err != nil {
		return nil, err
	}

	return clientset.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{})
}

// DeleteSecret deletes a secret.
func DeleteSecret(clientset kubernetes.Interface, namespace string, name string) error {
	return clientset.CoreV1().Secrets(namespace).Delete(name, &metav1.DeleteOptions{GracePeriodSeconds: &immediate})
}

// DeletePod deletes a pod.
func DeletePod(clientset kubernetes.Interface, namespace string, name string) error {
	return clientset.CoreV1().Pods(namespace).Delete(name, &metav1.DeleteOptions{GracePeriodSeconds: &immediate})
}

// CreateIstioCARole creates a role object named "istio-ca-role".
func CreateIstioCARole(clientset kubernetes.Interface, namespace string) error {
	role := rbac.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-ca-role",
			Namespace: namespace,
		},
		Rules: []rbac.PolicyRule{
			{
				Verbs:     []string{"create", "get", "watch", "list", "update"},
				APIGroups: []string{"core", ""},
				Resources: []string{"secrets"},
			},
			{
				Verbs:     []string{"get", "watch", "list"},
				APIGroups: []string{"core", ""},
				Resources: []string{"serviceaccounts"},
			},
		},
	}
	if _, err := clientset.RbacV1beta1().Roles(namespace).Create(&role); err != nil {
		return fmt.Errorf("failed to create role (error: %v)", err)
	}
	return nil
}

// CreateIstioCARoleBinding binds role "istio-ca-role" to default service account.
func CreateIstioCARoleBinding(clientset kubernetes.Interface, namespace string) error {
	rolebinding := rbac.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-ca-role-binding",
			Namespace: namespace,
		},
		Subjects: []rbac.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "default",
				Namespace: namespace,
			},
		},
		RoleRef: rbac.RoleRef{
			Kind:     "Role",
			Name:     "istio-ca-role",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	_, err := clientset.RbacV1beta1().RoleBindings(namespace).Create(&rolebinding)
	return err
}

func waitForServiceExternalIPAddress(clientset kubernetes.Interface, namespace string, uuid string,
	timeToWait time.Duration) error {
	selectors := labels.Set{"uuid": uuid}.AsSelectorPreValidated()
	listOptions := metav1.ListOptions{
		LabelSelector: selectors.String(),
	}

	watch, err := clientset.CoreV1().Services(namespace).Watch(listOptions)
	if err != nil {
		return fmt.Errorf("failed to set up a watch for service (error: %v)", err)
	}
	events := watch.ResultChan()

	startTime := time.Now()
	for {
		select {
		case event := <-events:
			svc := event.Object.(*v1.Service)
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				log.Infof("LoadBalancer for %v/%v is ready. IP: %v", namespace, svc.GetName(),
					svc.Status.LoadBalancer.Ingress[0].IP)
				return nil
			}
		case <-time.After(timeToWait - time.Since(startTime)):
			return fmt.Errorf("pod is not in running phase within %v", timeToWait)
		}
	}
}

func waitForPodRunning(clientset kubernetes.Interface, namespace string, uuid string,
	timeToWait time.Duration) error {
	selectors := labels.Set{"uuid": uuid}.AsSelectorPreValidated()
	listOptions := metav1.ListOptions{
		LabelSelector: selectors.String(),
	}
	watch, err := clientset.CoreV1().Pods(namespace).Watch(listOptions)
	if err != nil {
		return fmt.Errorf("failed to set up a watch for pod (error: %v)", err)
	}
	events := watch.ResultChan()

	startTime := time.Now()
	for {
		select {
		case event := <-events:
			pod := event.Object.(*v1.Pod)
			if pod.Status.Phase == v1.PodRunning {
				log.Infof("Pod %v/%v is in Running phase", namespace, pod.GetName())
				return nil
			}
		case <-time.After(timeToWait - time.Since(startTime)):
			return fmt.Errorf("pod is not in running phase within %v", timeToWait)
		}
	}
}

// WaitForSecretExist takes name of a secret and watches the secret. Returns the requested secret
// if it exists, or error on timeouts.
func WaitForSecretExist(clientset kubernetes.Interface, namespace string, secretName string,
	timeToWait time.Duration) (*v1.Secret, error) {
	watch, err := clientset.CoreV1().Secrets(namespace).Watch(metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to set up watch for secret (error: %v)", err)
	}
	events := watch.ResultChan()

	startTime := time.Now()
	for {
		select {
		case event := <-events:
			secret := event.Object.(*v1.Secret)
			if secret.GetName() == secretName {
				return secret, nil
			}
		case <-time.After(timeToWait - time.Since(startTime)):
			return nil, fmt.Errorf("secret %v/%v did not become existent within %v",
				namespace, secretName, timeToWait)
		}
	}
}
