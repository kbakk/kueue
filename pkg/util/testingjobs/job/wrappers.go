/*
Copyright 2022 The Kubernetes Authors.

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

package testing

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/util/pointer"
)

// JobWrapper wraps a Job.
type JobWrapper struct{ batchv1.Job }

// MakeJob creates a wrapper for a suspended job with a single container and parallelism=1.
func MakeJob(name, ns string) *JobWrapper {
	return &JobWrapper{batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: make(map[string]string, 1),
		},
		Spec: batchv1.JobSpec{
			Parallelism: pointer.Int32(1),
			Suspend:     pointer.Bool(true),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:      "c",
							Image:     "pause",
							Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}},
						},
					},
					NodeSelector: map[string]string{},
				},
			},
		},
	}}
}

// Obj returns the inner Job.
func (j *JobWrapper) Obj() *batchv1.Job {
	return &j.Job
}

// Clone returns deep copy of the Job.
func (j *JobWrapper) Clone() *JobWrapper {
	return &JobWrapper{Job: *j.DeepCopy()}
}

// Suspend updates the suspend status of the job
func (j *JobWrapper) Suspend(s bool) *JobWrapper {
	j.Spec.Suspend = pointer.Bool(s)
	return j
}

// Parallelism updates job parallelism.
func (j *JobWrapper) Parallelism(p int32) *JobWrapper {
	j.Spec.Parallelism = pointer.Int32(p)
	return j
}

// Completions updates job completions.
func (j *JobWrapper) Completions(p int32) *JobWrapper {
	j.Spec.Completions = pointer.Int32(p)
	return j
}

// Indexed sets the job's completion to Indexed of NonIndexed
func (j *JobWrapper) Indexed(indexed bool) *JobWrapper {
	mode := batchv1.NonIndexedCompletion
	if indexed {
		mode = batchv1.IndexedCompletion
	}
	j.Spec.CompletionMode = &mode
	return j
}

// PriorityClass updates job priorityclass.
func (j *JobWrapper) PriorityClass(pc string) *JobWrapper {
	j.Spec.Template.Spec.PriorityClassName = pc
	return j
}

// Queue updates the queue name of the job
func (j *JobWrapper) Queue(queue string) *JobWrapper {
	if j.Labels == nil {
		j.Labels = make(map[string]string)
	}
	j.Labels[constants.QueueLabel] = queue
	return j
}

// QueueNameAnnotation updates the queue name of the job by annotation (deprecated)
func (j *JobWrapper) QueueNameAnnotation(queue string) *JobWrapper {
	j.Annotations[constants.QueueAnnotation] = queue
	return j
}

// ParentWorkload sets the parent-workload annotation
func (j *JobWrapper) ParentWorkload(parentWorkload string) *JobWrapper {
	j.Annotations[constants.ParentWorkloadAnnotation] = parentWorkload
	return j
}

func (j *JobWrapper) SetAnnotation(key, content string) *JobWrapper {
	j.Annotations[key] = content
	return j
}

// Toleration adds a toleration to the job.
func (j *JobWrapper) Toleration(t corev1.Toleration) *JobWrapper {
	j.Spec.Template.Spec.Tolerations = append(j.Spec.Template.Spec.Tolerations, t)
	return j
}

// NodeSelector adds a node selector to the job.
func (j *JobWrapper) NodeSelector(k, v string) *JobWrapper {
	j.Spec.Template.Spec.NodeSelector[k] = v
	return j
}

// Request adds a resource request to the default container.
func (j *JobWrapper) Request(r corev1.ResourceName, v string) *JobWrapper {
	j.Spec.Template.Spec.Containers[0].Resources.Requests[r] = resource.MustParse(v)
	return j
}

func (j *JobWrapper) Image(image string, args []string) *JobWrapper {
	j.Spec.Template.Spec.Containers[0].Image = image
	j.Spec.Template.Spec.Containers[0].Args = args
	return j
}

// OwnerReference adds a ownerReference to the default container.
func (j *JobWrapper) OwnerReference(ownerName string, ownerGVK schema.GroupVersionKind) *JobWrapper {
	j.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: ownerGVK.GroupVersion().String(),
			Kind:       ownerGVK.Kind,
			Name:       ownerName,
			UID:        types.UID(ownerName),
			Controller: pointer.Bool(true),
		},
	}
	return j
}

// UID updates the uid of the job.
func (j *JobWrapper) UID(uid string) *JobWrapper {
	j.ObjectMeta.UID = types.UID(uid)
	return j
}

// StartTime sets the .status.startTime
func (j *JobWrapper) StartTime(t time.Time) *JobWrapper {
	j.Status.StartTime = &metav1.Time{Time: t}
	return j
}

// Active sets the .status.active
func (j *JobWrapper) Active(c int32) *JobWrapper {
	j.Status.Active = c
	return j
}
