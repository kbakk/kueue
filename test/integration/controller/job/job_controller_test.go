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

package job

import (
	"fmt"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	workloadjob "sigs.k8s.io/kueue/pkg/controller/jobs/job"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/util/pointer"
	"sigs.k8s.io/kueue/pkg/util/testing"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	"sigs.k8s.io/kueue/pkg/workload"
	"sigs.k8s.io/kueue/test/integration/framework"
	"sigs.k8s.io/kueue/test/util"
)

const (
	parallelism       = 4
	jobName           = "test-job"
	labelKey          = "cloud.provider.com/instance"
	priorityClassName = "test-priority-class"
	priorityValue     = 10
	parentJobName     = jobName + "-parent"
	childJobName      = jobName + "-child"
)

var (
	ignoreConditionTimestamps = cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime")
)

// +kubebuilder:docs-gen:collapse=Imports

var _ = ginkgo.Describe("Job controller", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {

	ginkgo.BeforeAll(func() {
		fwk = &framework.Framework{
			ManagerSetup: managerSetup(jobframework.WithManageJobsWithoutQueueName(true)),
			CRDPath:      crdPath,
		}
		ctx, cfg, k8sClient = fwk.Setup()
	})
	ginkgo.AfterAll(func() {
		fwk.Teardown()
	})

	var (
		ns                *corev1.Namespace
		wlLookupKey       types.NamespacedName
		parentWlLookupKey types.NamespacedName
		childLookupKey    types.NamespacedName
	)

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "core-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		wlLookupKey = types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(jobName), Namespace: ns.Name}
		parentWlLookupKey = types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(parentJobName), Namespace: ns.Name}
		childLookupKey = types.NamespacedName{Name: childJobName, Namespace: ns.Name}
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("Should reconcile workload and job for all jobs", func() {
		ginkgo.By("checking the job gets suspended when created unsuspended")
		priorityClass := testing.MakePriorityClass(priorityClassName).
			PriorityValue(int32(priorityValue)).Obj()
		gomega.Expect(k8sClient.Create(ctx, priorityClass)).Should(gomega.Succeed())
		ginkgo.DeferCleanup(func() {
			gomega.Expect(k8sClient.Delete(ctx, priorityClass)).To(gomega.Succeed())
		})
		job := testingjob.MakeJob(jobName, ns.Name).PriorityClass(priorityClassName).Obj()
		gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())
		lookupKey := types.NamespacedName{Name: jobName, Namespace: ns.Name}
		createdJob := &batchv1.Job{}
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(ctx, lookupKey, createdJob); err != nil {
				return false
			}
			return createdJob.Spec.Suspend != nil && *createdJob.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())

		ginkgo.By("checking the workload is created without queue assigned")
		createdWorkload := &kueue.Workload{}
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, wlLookupKey, createdWorkload)
			return err == nil
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		gomega.Expect(createdWorkload.Spec.QueueName).Should(gomega.Equal(""), "The Workload shouldn't have .spec.queueName set")
		gomega.Expect(metav1.IsControlledBy(createdWorkload, job)).To(gomega.BeTrue(), "The Workload should be owned by the Job")

		ginkgo.By("checking the workload is created with priority and priorityName")
		gomega.Expect(createdWorkload.Spec.PriorityClassName).Should(gomega.Equal(priorityClassName))
		gomega.Expect(*createdWorkload.Spec.Priority).Should(gomega.Equal(int32(priorityValue)))

		ginkgo.By("checking the workload is updated with queue name when the job does")
		jobQueueName := "test-queue"
		createdJob.Annotations = map[string]string{constants.QueueAnnotation: jobQueueName}
		gomega.Expect(k8sClient.Update(ctx, createdJob)).Should(gomega.Succeed())
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(ctx, wlLookupKey, createdWorkload); err != nil {
				return false
			}
			return createdWorkload.Spec.QueueName == jobQueueName
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())

		ginkgo.By("checking a second non-matching workload is deleted")
		secondWl := &kueue.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workloadjob.GetWorkloadNameForJob("second-workload"),
				Namespace: createdWorkload.Namespace,
			},
			Spec: *createdWorkload.Spec.DeepCopy(),
		}
		gomega.Expect(ctrl.SetControllerReference(createdJob, secondWl, scheme.Scheme)).Should(gomega.Succeed())
		secondWl.Spec.PodSets[0].Count += 1
		gomega.Expect(k8sClient.Create(ctx, secondWl)).Should(gomega.Succeed())
		gomega.Eventually(func() error {
			wl := &kueue.Workload{}
			key := types.NamespacedName{Name: secondWl.Name, Namespace: secondWl.Namespace}
			return k8sClient.Get(ctx, key, wl)
		}, util.Timeout, util.Interval).Should(testing.BeNotFoundError())
		// check the original wl is still there
		gomega.Consistently(func() bool {
			err := k8sClient.Get(ctx, wlLookupKey, createdWorkload)
			return err == nil
		}, util.ConsistentDuration, util.Interval).Should(gomega.BeTrue())
		gomega.Eventually(func() bool {
			ok, _ := testing.CheckLatestEvent(ctx, k8sClient, "DeletedWorkload", corev1.EventTypeNormal, fmt.Sprintf("Deleted not matching Workload: %v", workload.Key(secondWl)))
			return ok
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())

		ginkgo.By("checking the job is unsuspended when workload is assigned")
		onDemandFlavor := testing.MakeResourceFlavor("on-demand").Label(labelKey, "on-demand").Obj()
		gomega.Expect(k8sClient.Create(ctx, onDemandFlavor)).Should(gomega.Succeed())
		spotFlavor := testing.MakeResourceFlavor("spot").Label(labelKey, "spot").Obj()
		gomega.Expect(k8sClient.Create(ctx, spotFlavor)).Should(gomega.Succeed())
		clusterQueue := testing.MakeClusterQueue("cluster-queue").
			ResourceGroup(
				*testing.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "5").Obj(),
				*testing.MakeFlavorQuotas("spot").Resource(corev1.ResourceCPU, "5").Obj(),
			).Obj()
		admission := testing.MakeAdmission(clusterQueue.Name).
			Assignment(corev1.ResourceCPU, "on-demand", "1m").
			AssignmentPodCount(createdWorkload.Spec.PodSets[0].Count).
			Obj()
		gomega.Expect(util.SetAdmission(ctx, k8sClient, createdWorkload, admission)).Should(gomega.Succeed())
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(ctx, lookupKey, createdJob); err != nil {
				return false
			}
			return !*createdJob.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		gomega.Eventually(func() bool {
			ok, _ := testing.CheckLatestEvent(ctx, k8sClient, "Started", corev1.EventTypeNormal, fmt.Sprintf("Admitted by clusterQueue %v", clusterQueue.Name))
			return ok
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		gomega.Expect(len(createdJob.Spec.Template.Spec.NodeSelector)).Should(gomega.Equal(1))
		gomega.Expect(createdJob.Spec.Template.Spec.NodeSelector[labelKey]).Should(gomega.Equal(onDemandFlavor.Name))
		gomega.Consistently(func() bool {
			if err := k8sClient.Get(ctx, wlLookupKey, createdWorkload); err != nil {
				return false
			}
			return len(createdWorkload.Status.Conditions) == 1
		}, util.ConsistentDuration, util.Interval).Should(gomega.BeTrue())

		// We need to set startTime to the job since the kube-controller-manager doesn't exist in envtest.
		ginkgo.By("setting startTime to the job")
		now := metav1.Now()
		createdJob.Status.StartTime = &now
		gomega.Expect(k8sClient.Status().Update(ctx, createdJob)).Should(gomega.Succeed())

		ginkgo.By("checking the job gets suspended when parallelism changes and the added node selectors are removed")
		newParallelism := int32(parallelism + 1)
		createdJob.Spec.Parallelism = &newParallelism
		gomega.Expect(k8sClient.Update(ctx, createdJob)).Should(gomega.Succeed())
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(ctx, lookupKey, createdJob); err != nil {
				return false
			}
			return createdJob.Spec.Suspend != nil && *createdJob.Spec.Suspend && createdJob.Status.StartTime == nil &&
				len(createdJob.Spec.Template.Spec.NodeSelector) == 0
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		gomega.Eventually(func() bool {
			ok, _ := testing.CheckLatestEvent(ctx, k8sClient, "DeletedWorkload", corev1.EventTypeNormal, fmt.Sprintf("Deleted not matching Workload: %v", wlLookupKey.String()))
			return ok
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())

		ginkgo.By("checking the workload is updated with new count")
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(ctx, wlLookupKey, createdWorkload); err != nil {
				return false
			}
			return createdWorkload.Spec.PodSets[0].Count == newParallelism
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		gomega.Expect(createdWorkload.Status.Admission).Should(gomega.BeNil())

		ginkgo.By("checking the job is unsuspended and selectors added when workload is assigned again")
		admission = testing.MakeAdmission(clusterQueue.Name).
			Assignment(corev1.ResourceCPU, "spot", "1m").
			AssignmentPodCount(createdWorkload.Spec.PodSets[0].Count).
			Obj()
		gomega.Expect(util.SetAdmission(ctx, k8sClient, createdWorkload, admission)).Should(gomega.Succeed())
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(ctx, lookupKey, createdJob); err != nil {
				return false
			}
			return !*createdJob.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		gomega.Expect(len(createdJob.Spec.Template.Spec.NodeSelector)).Should(gomega.Equal(1))
		gomega.Expect(createdJob.Spec.Template.Spec.NodeSelector[labelKey]).Should(gomega.Equal(spotFlavor.Name))
		gomega.Consistently(func() bool {
			if err := k8sClient.Get(ctx, wlLookupKey, createdWorkload); err != nil {
				return false
			}
			return len(createdWorkload.Status.Conditions) == 1
		}, util.ConsistentDuration, util.Interval).Should(gomega.BeTrue())

		ginkgo.By("checking the workload is finished when job is completed")
		createdJob.Status.Conditions = append(createdJob.Status.Conditions,
			batchv1.JobCondition{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastProbeTime:      metav1.Now(),
				LastTransitionTime: metav1.Now(),
			})
		gomega.Expect(k8sClient.Status().Update(ctx, createdJob)).Should(gomega.Succeed())
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, wlLookupKey, createdWorkload)
			if err != nil || len(createdWorkload.Status.Conditions) == 1 {
				return false
			}

			return apimeta.IsStatusConditionTrue(createdWorkload.Status.Conditions, kueue.WorkloadFinished)
		}, util.Timeout, util.Interval).Should(gomega.BeTrue())
	})

	ginkgo.It("Should reconcile job when queueName set by annotation (deprecated)", func() {
		ginkgo.By("checking the workload is created with correct queue name assigned")
		jobQueueName := "test-queue"
		job := testingjob.MakeJob(jobName, ns.Name).QueueNameAnnotation("test-queue").Obj()
		gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())
		createdWorkload := &kueue.Workload{}
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, wlLookupKey, createdWorkload)
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
		gomega.Expect(createdWorkload.Spec.QueueName).Should(gomega.Equal(jobQueueName))
	})

	ginkgo.When("The parent-workload annotation is used", func() {

		ginkgo.It("Should suspend a job if the parent workload does not exist", func() {
			ginkgo.By("Creating the child job which uses the parent workload annotation")
			childJob := testingjob.MakeJob(childJobName, ns.Name).Suspend(false).ParentWorkload("non-existing-parent-workload").Obj()
			gomega.Expect(k8sClient.Create(ctx, childJob)).Should(gomega.Succeed())

			ginkgo.By("checking that the child job is suspended")
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, childLookupKey, childJob)).Should(gomega.Succeed())
				return childJob.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
		})

		ginkgo.It("Should not create child workload for a job with parent-workload annotation", func() {
			ginkgo.By("creating the parent job")
			parentJob := testingjob.MakeJob(parentJobName, ns.Name).Obj()
			gomega.Expect(k8sClient.Create(ctx, parentJob)).Should(gomega.Succeed())

			ginkgo.By("waiting for the parent workload to be created")
			parentWorkload := &kueue.Workload{}
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, parentWlLookupKey, parentWorkload)
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("Creating the child job which uses the parent workload annotation")
			childJob := testingjob.MakeJob(childJobName, ns.Name).ParentWorkload(parentWorkload.Name).Obj()
			gomega.Expect(k8sClient.Create(ctx, childJob)).Should(gomega.Succeed())

			ginkgo.By("Checking that the child workload is not created")
			childWorkload := &kueue.Workload{}
			childWlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(childJobName), Namespace: ns.Name}
			gomega.Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, childWlLookupKey, childWorkload))
			}, util.ConsistentDuration, util.Interval).Should(gomega.BeTrue())
		})

		ginkgo.It("Should not update the queue name of the workload with an empty value that the child job has", func() {
			jobQueueName := "test-queue"

			ginkgo.By("creating the parent job with queue name")
			parentJob := testingjob.MakeJob(parentJobName, ns.Name).Queue(jobQueueName).Obj()
			gomega.Expect(k8sClient.Create(ctx, parentJob)).Should(gomega.Succeed())

			ginkgo.By("waiting for the parent workload to be created")
			parentWorkload := &kueue.Workload{}
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, parentWlLookupKey, parentWorkload)
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("Creating the child job which uses the parent workload annotation")
			childJob := testingjob.MakeJob(childJobName, ns.Name).ParentWorkload(parentWorkload.Name).Obj()
			gomega.Expect(k8sClient.Create(ctx, childJob)).Should(gomega.Succeed())

			ginkgo.By("Checking that the queue name of the parent workload isn't updated with an empty value")
			parentWorkload = &kueue.Workload{}
			gomega.Consistently(func() bool {
				if err := k8sClient.Get(ctx, parentWlLookupKey, parentWorkload); err != nil {
					return true
				}
				return parentWorkload.Spec.QueueName == jobQueueName
			}, util.ConsistentDuration, util.Interval).Should(gomega.BeTrue())
		})

		ginkgo.It("Should change the suspension status of the child job when the parent workload is not admitted", func() {
			ginkgo.By("Create a resource flavor")
			defaultFlavor := testing.MakeResourceFlavor("default").Label(labelKey, "default").Obj()
			gomega.Expect(k8sClient.Create(ctx, defaultFlavor)).Should(gomega.Succeed())

			ginkgo.By("creating the parent job")
			parentJob := testingjob.MakeJob(parentJobName, ns.Name).Obj()
			gomega.Expect(k8sClient.Create(ctx, parentJob)).Should(gomega.Succeed())

			ginkgo.By("waiting for the parent workload to be created")
			parentWorkload := &kueue.Workload{}
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, parentWlLookupKey, parentWorkload)
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("Creating the child job with the parent-workload annotation")
			childJob := testingjob.MakeJob(childJobName, ns.Name).ParentWorkload(parentWlLookupKey.Name).Suspend(false).Obj()
			gomega.Expect(k8sClient.Create(ctx, childJob)).Should(gomega.Succeed())

			ginkgo.By("checking that the child job is suspended")
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, childLookupKey, childJob)).Should(gomega.Succeed())
				return childJob.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
		})
	})

	ginkgo.It("Should finish the preemption when the job becomes inactive", func() {
		job := testingjob.MakeJob(jobName, ns.Name).Queue("q").Obj()
		wl := &kueue.Workload{}
		ginkgo.By("create the job and admit the workload", func() {
			gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())
			gomega.Eventually(func() error { return k8sClient.Get(ctx, wlLookupKey, wl) }, util.Timeout, util.Interval).Should(gomega.Succeed())
			admission := testing.MakeAdmission("q", job.Spec.Template.Spec.Containers[0].Name).Obj()
			gomega.Expect(util.SetAdmission(ctx, k8sClient, wl, admission)).To(gomega.Succeed())
		})

		ginkgo.By("wait for the job to be unsuspended", func() {
			gomega.Eventually(func() bool {
				gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(gomega.Succeed())
				return *job.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.BeFalse())
		})

		ginkgo.By("mark the job as active", func() {
			gomega.Eventually(func() error {
				gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(gomega.Succeed())
				job.Status.Active = 1
				return k8sClient.Status().Update(ctx, job)
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.By("preempt the workload", func() {
			gomega.Eventually(func() error {
				gomega.Expect(k8sClient.Get(ctx, wlLookupKey, wl)).To(gomega.Succeed())
				return workload.UpdateStatus(ctx, k8sClient, wl, kueue.WorkloadEvicted, metav1.ConditionTrue, kueue.WorkloadEvictedByPreemption, "By test", "evict")
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.By("wait for the job to be suspended", func() {
			gomega.Eventually(func() bool {
				gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(gomega.Succeed())
				return *job.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.BeTrue())
		})

		ginkgo.By("the workload should stay admitted", func() {
			gomega.Consistently(func() bool {
				gomega.Expect(k8sClient.Get(ctx, wlLookupKey, wl)).To(gomega.Succeed())
				return apimeta.IsStatusConditionTrue(wl.Status.Conditions, kueue.WorkloadAdmitted)
			}, util.ConsistentDuration, util.Interval).Should(gomega.BeTrue())
		})

		ginkgo.By("mark the job as inactive", func() {
			gomega.Eventually(func() error {
				gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(gomega.Succeed())
				job.Status.Active = 0
				return k8sClient.Status().Update(ctx, job)
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.By("the workload should get unadmitted", func() {
			util.ExpectWorkloadsToBePending(ctx, k8sClient, wl)
		})
	})
})

var _ = ginkgo.Describe("Job controller when waitForPodsReady enabled", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
	type podsReadyTestSpec struct {
		beforeJobStatus *batchv1.JobStatus
		beforeCondition *metav1.Condition
		jobStatus       batchv1.JobStatus
		suspended       bool
		wantCondition   *metav1.Condition
	}

	var (
		ns            *corev1.Namespace
		defaultFlavor = testing.MakeResourceFlavor("default").Label(labelKey, "default").Obj()
		wlLookupKey   types.NamespacedName
	)

	ginkgo.BeforeAll(func() {
		fwk = &framework.Framework{
			ManagerSetup: managerSetup(jobframework.WithWaitForPodsReady(true)),
			CRDPath:      crdPath,
		}
		ctx, cfg, k8sClient = fwk.Setup()
		ginkgo.By("Create a resource flavor")
		gomega.Expect(k8sClient.Create(ctx, defaultFlavor)).Should(gomega.Succeed())
	})
	ginkgo.AfterAll(func() {
		util.ExpectResourceFlavorToBeDeleted(ctx, k8sClient, defaultFlavor, true)
		fwk.Teardown()
	})

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "core-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		wlLookupKey = types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(jobName), Namespace: ns.Name}
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.DescribeTable("Single job at different stages of progress towards completion",
		func(podsReadyTestSpec podsReadyTestSpec) {
			ginkgo.By("Create a job")
			job := testingjob.MakeJob(jobName, ns.Name).Parallelism(2).Obj()
			jobQueueName := "test-queue"
			job.Annotations = map[string]string{constants.QueueAnnotation: jobQueueName}
			gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())
			lookupKey := types.NamespacedName{Name: jobName, Namespace: ns.Name}
			createdJob := &batchv1.Job{}
			gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())

			ginkgo.By("Fetch the workload created for the job")
			createdWorkload := &kueue.Workload{}
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, wlLookupKey, createdWorkload)
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("Admit the workload created for the job")
			admission := testing.MakeAdmission("foo").
				Assignment(corev1.ResourceCPU, "default", "1m").
				AssignmentPodCount(createdWorkload.Spec.PodSets[0].Count).
				Obj()
			gomega.Expect(util.SetAdmission(ctx, k8sClient, createdWorkload, admission)).Should(gomega.Succeed())
			gomega.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())

			ginkgo.By("Await for the job to be unsuspended")
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())
				return createdJob.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))

			if podsReadyTestSpec.beforeJobStatus != nil {
				ginkgo.By("Update the job status to simulate its initial progress towards completion")
				createdJob.Status = *podsReadyTestSpec.beforeJobStatus
				gomega.Expect(k8sClient.Status().Update(ctx, createdJob)).Should(gomega.Succeed())
				gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())
			}

			if podsReadyTestSpec.beforeCondition != nil {
				ginkgo.By("Update the workload status")
				gomega.Eventually(func() *metav1.Condition {
					gomega.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					return apimeta.FindStatusCondition(createdWorkload.Status.Conditions, kueue.WorkloadPodsReady)
				}, util.Timeout, util.Interval).Should(gomega.BeComparableTo(podsReadyTestSpec.beforeCondition, ignoreConditionTimestamps))
			}

			ginkgo.By("Update the job status to simulate its progress towards completion")
			createdJob.Status = podsReadyTestSpec.jobStatus
			gomega.Expect(k8sClient.Status().Update(ctx, createdJob)).Should(gomega.Succeed())
			gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())

			if podsReadyTestSpec.suspended {
				ginkgo.By("Unset admission of the workload to suspend the job")
				gomega.Eventually(func() error {
					// the update may need to be retried due to a conflict as the workload gets
					// also updated due to setting of the job status.
					if err := k8sClient.Get(ctx, wlLookupKey, createdWorkload); err != nil {
						return err
					}
					return util.SetAdmission(ctx, k8sClient, createdWorkload, nil)
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			}

			ginkgo.By("Verify the PodsReady condition is added")
			gomega.Eventually(func() *metav1.Condition {
				gomega.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
				return apimeta.FindStatusCondition(createdWorkload.Status.Conditions, kueue.WorkloadPodsReady)
			}, util.Timeout, util.Interval).Should(gomega.BeComparableTo(podsReadyTestSpec.wantCondition, ignoreConditionTimestamps))
		},
		ginkgo.Entry("No progress", podsReadyTestSpec{
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PodsReady",
				Message: "Not all pods are ready or succeeded",
			},
		}),
		ginkgo.Entry("Single pod ready", podsReadyTestSpec{
			jobStatus: batchv1.JobStatus{
				Ready: pointer.Int32(1),
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PodsReady",
				Message: "Not all pods are ready or succeeded",
			},
		}),
		ginkgo.Entry("Single pod succeeded", podsReadyTestSpec{
			jobStatus: batchv1.JobStatus{
				Succeeded: 1,
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PodsReady",
				Message: "Not all pods are ready or succeeded",
			},
		}),
		ginkgo.Entry("All pods are ready", podsReadyTestSpec{
			jobStatus: batchv1.JobStatus{
				Ready: pointer.Int32(2),
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
		}),
		ginkgo.Entry("One pod ready, one succeeded", podsReadyTestSpec{
			jobStatus: batchv1.JobStatus{
				Ready:     pointer.Int32(1),
				Succeeded: 1,
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
		}),
		ginkgo.Entry("All pods are succeeded", podsReadyTestSpec{
			jobStatus: batchv1.JobStatus{
				Ready:     pointer.Int32(0),
				Succeeded: 2,
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
		}),
		ginkgo.Entry("All pods are succeeded; PodsReady=False before", podsReadyTestSpec{
			beforeCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PodsReady",
				Message: "Not all pods are ready or succeeded",
			},
			jobStatus: batchv1.JobStatus{
				Ready:     pointer.Int32(0),
				Succeeded: 2,
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
		}),
		ginkgo.Entry("One ready pod, one failed; PodsReady=True before", podsReadyTestSpec{
			beforeJobStatus: &batchv1.JobStatus{
				Ready: pointer.Int32(2),
			},
			beforeCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
			jobStatus: batchv1.JobStatus{
				Ready:  pointer.Int32(1),
				Failed: 1,
			},
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
		}),
		ginkgo.Entry("Job suspended without ready pods; but PodsReady=True before", podsReadyTestSpec{
			beforeJobStatus: &batchv1.JobStatus{
				Ready: pointer.Int32(2),
			},
			beforeCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
			jobStatus: batchv1.JobStatus{
				Failed: 2,
			},
			suspended: true,
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PodsReady",
				Message: "Not all pods are ready or succeeded",
			},
		}),
		ginkgo.Entry("Job suspended with all pods ready; PodsReady=True before", podsReadyTestSpec{
			beforeJobStatus: &batchv1.JobStatus{
				Ready: pointer.Int32(2),
			},
			beforeCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionTrue,
				Reason:  "PodsReady",
				Message: "All pods were ready or succeeded since the workload admission",
			},
			jobStatus: batchv1.JobStatus{
				Ready: pointer.Int32(2),
			},
			suspended: true,
			wantCondition: &metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PodsReady",
				Message: "Not all pods are ready or succeeded",
			},
		}),
	)
})

var _ = ginkgo.Describe("Job controller interacting with scheduler", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
	const (
		instanceKey = "cloud.provider.com/instance"
	)

	var (
		ns                  *corev1.Namespace
		onDemandFlavor      *kueue.ResourceFlavor
		spotTaintedFlavor   *kueue.ResourceFlavor
		spotUntaintedFlavor *kueue.ResourceFlavor
		prodClusterQ        *kueue.ClusterQueue
		devClusterQ         *kueue.ClusterQueue
		podsCountClusterQ   *kueue.ClusterQueue
		prodLocalQ          *kueue.LocalQueue
		devLocalQ           *kueue.LocalQueue
	)

	ginkgo.BeforeAll(func() {
		fwk = &framework.Framework{
			ManagerSetup: managerAndSchedulerSetup(),
			CRDPath:      crdPath,
		}
		ctx, cfg, k8sClient = fwk.Setup()
	})
	ginkgo.AfterAll(func() {
		fwk.Teardown()
	})

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "core-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		onDemandFlavor = testing.MakeResourceFlavor("on-demand").Label(instanceKey, "on-demand").Obj()
		gomega.Expect(k8sClient.Create(ctx, onDemandFlavor)).Should(gomega.Succeed())

		spotTaintedFlavor = testing.MakeResourceFlavor("spot-tainted").
			Label(instanceKey, "spot-tainted").
			Taint(corev1.Taint{
				Key:    instanceKey,
				Value:  "spot-tainted",
				Effect: corev1.TaintEffectNoSchedule,
			}).Obj()
		gomega.Expect(k8sClient.Create(ctx, spotTaintedFlavor)).Should(gomega.Succeed())

		spotUntaintedFlavor = testing.MakeResourceFlavor("spot-untainted").Label(instanceKey, "spot-untainted").Obj()
		gomega.Expect(k8sClient.Create(ctx, spotUntaintedFlavor)).Should(gomega.Succeed())

		prodClusterQ = testing.MakeClusterQueue("prod-cq").
			Cohort("prod").
			ResourceGroup(
				*testing.MakeFlavorQuotas("spot-tainted").Resource(corev1.ResourceCPU, "5", "0").Obj(),
				*testing.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "5").Obj(),
			).Obj()
		gomega.Expect(k8sClient.Create(ctx, prodClusterQ)).Should(gomega.Succeed())

		devClusterQ = testing.MakeClusterQueue("dev-clusterqueue").
			ResourceGroup(
				*testing.MakeFlavorQuotas("spot-untainted").Resource(corev1.ResourceCPU, "5").Obj(),
				*testing.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "5").Obj(),
			).
			Preemption(kueue.ClusterQueuePreemption{
				WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
			}).
			Obj()
		gomega.Expect(k8sClient.Create(ctx, devClusterQ)).Should(gomega.Succeed())
		podsCountClusterQ = testing.MakeClusterQueue("pods-clusterqueue").
			ResourceGroup(
				*testing.MakeFlavorQuotas("on-demand").Resource(corev1.ResourcePods, "5").Obj(),
			).
			Obj()
		gomega.Expect(k8sClient.Create(ctx, podsCountClusterQ)).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		util.ExpectClusterQueueToBeDeleted(ctx, k8sClient, prodClusterQ, true)
		util.ExpectClusterQueueToBeDeleted(ctx, k8sClient, devClusterQ, true)
		util.ExpectClusterQueueToBeDeleted(ctx, k8sClient, podsCountClusterQ, true)
		util.ExpectResourceFlavorToBeDeleted(ctx, k8sClient, onDemandFlavor, true)
		gomega.Expect(util.DeleteResourceFlavor(ctx, k8sClient, spotTaintedFlavor)).To(gomega.Succeed())
		gomega.Expect(util.DeleteResourceFlavor(ctx, k8sClient, spotUntaintedFlavor)).To(gomega.Succeed())
	})

	ginkgo.It("Should schedule jobs as they fit in their ClusterQueue", func() {
		ginkgo.By("creating localQueues")
		prodLocalQ = testing.MakeLocalQueue("prod-queue", ns.Name).ClusterQueue(prodClusterQ.Name).Obj()
		gomega.Expect(k8sClient.Create(ctx, prodLocalQ)).Should(gomega.Succeed())
		devLocalQ = testing.MakeLocalQueue("dev-queue", ns.Name).ClusterQueue(devClusterQ.Name).Obj()
		gomega.Expect(k8sClient.Create(ctx, devLocalQ)).Should(gomega.Succeed())

		ginkgo.By("checking the first prod job starts")
		prodJob1 := testingjob.MakeJob("prod-job1", ns.Name).Queue(prodLocalQ.Name).Request(corev1.ResourceCPU, "2").Obj()
		gomega.Expect(k8sClient.Create(ctx, prodJob1)).Should(gomega.Succeed())
		lookupKey1 := types.NamespacedName{Name: prodJob1.Name, Namespace: prodJob1.Namespace}
		createdProdJob1 := &batchv1.Job{}
		gomega.Eventually(func() *bool {
			gomega.Expect(k8sClient.Get(ctx, lookupKey1, createdProdJob1)).Should(gomega.Succeed())
			return createdProdJob1.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
		gomega.Expect(createdProdJob1.Spec.Template.Spec.NodeSelector[instanceKey]).Should(gomega.Equal(onDemandFlavor.Name))
		util.ExpectPendingWorkloadsMetric(prodClusterQ, 0, 0)
		util.ExpectAdmittedActiveWorkloadsMetric(prodClusterQ, 1)

		ginkgo.By("checking a second no-fit prod job does not start")
		prodJob2 := testingjob.MakeJob("prod-job2", ns.Name).Queue(prodLocalQ.Name).Request(corev1.ResourceCPU, "5").Obj()
		gomega.Expect(k8sClient.Create(ctx, prodJob2)).Should(gomega.Succeed())
		lookupKey2 := types.NamespacedName{Name: prodJob2.Name, Namespace: prodJob2.Namespace}
		createdProdJob2 := &batchv1.Job{}
		gomega.Consistently(func() *bool {
			gomega.Expect(k8sClient.Get(ctx, lookupKey2, createdProdJob2)).Should(gomega.Succeed())
			return createdProdJob2.Spec.Suspend
		}, util.ConsistentDuration, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
		util.ExpectPendingWorkloadsMetric(prodClusterQ, 0, 1)
		util.ExpectAdmittedActiveWorkloadsMetric(prodClusterQ, 1)

		ginkgo.By("checking a dev job starts")
		devJob := testingjob.MakeJob("dev-job", ns.Name).Queue(devLocalQ.Name).Request(corev1.ResourceCPU, "5").Obj()
		gomega.Expect(k8sClient.Create(ctx, devJob)).Should(gomega.Succeed())
		createdDevJob := &batchv1.Job{}
		gomega.Eventually(func() *bool {
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: devJob.Name, Namespace: devJob.Namespace}, createdDevJob)).
				Should(gomega.Succeed())
			return createdDevJob.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
		gomega.Expect(createdDevJob.Spec.Template.Spec.NodeSelector[instanceKey]).Should(gomega.Equal(spotUntaintedFlavor.Name))
		util.ExpectPendingWorkloadsMetric(devClusterQ, 0, 0)
		util.ExpectAdmittedActiveWorkloadsMetric(devClusterQ, 1)

		ginkgo.By("checking the second prod job starts when the first finishes")
		createdProdJob1.Status.Conditions = append(createdProdJob1.Status.Conditions,
			batchv1.JobCondition{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastProbeTime:      metav1.Now(),
				LastTransitionTime: metav1.Now(),
			})
		gomega.Expect(k8sClient.Status().Update(ctx, createdProdJob1)).Should(gomega.Succeed())
		gomega.Eventually(func() *bool {
			gomega.Expect(k8sClient.Get(ctx, lookupKey2, createdProdJob2)).Should(gomega.Succeed())
			return createdProdJob2.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
		gomega.Expect(createdProdJob2.Spec.Template.Spec.NodeSelector[instanceKey]).Should(gomega.Equal(onDemandFlavor.Name))
		util.ExpectPendingWorkloadsMetric(prodClusterQ, 0, 0)
		util.ExpectAdmittedActiveWorkloadsMetric(prodClusterQ, 1)
	})

	ginkgo.It("Should unsuspend job iff localQueue is in the same namespace", func() {
		ginkgo.By("create another namespace")
		ns2 := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "e2e-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns2)).To(gomega.Succeed())
		defer func() {
			gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns2)).To(gomega.Succeed())
		}()

		ginkgo.By("create a localQueue located in a different namespace as the job")
		localQueue := testing.MakeLocalQueue("local-queue", ns2.Name).Obj()
		localQueue.Spec.ClusterQueue = kueue.ClusterQueueReference(prodClusterQ.Name)

		ginkgo.By("create a job")
		prodJob := testingjob.MakeJob("prod-job", ns.Name).Queue(localQueue.Name).Request(corev1.ResourceCPU, "2").Obj()
		gomega.Expect(k8sClient.Create(ctx, prodJob)).Should(gomega.Succeed())

		ginkgo.By("job should be suspend")
		lookupKey := types.NamespacedName{Name: prodJob.Name, Namespace: prodJob.Namespace}
		createdProdJob := &batchv1.Job{}
		gomega.Eventually(func() *bool {
			gomega.Expect(k8sClient.Get(ctx, lookupKey, createdProdJob)).Should(gomega.Succeed())
			return createdProdJob.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(true)))

		ginkgo.By("creating another localQueue of the same name and in the same namespace as the job")
		prodLocalQ = testing.MakeLocalQueue(localQueue.Name, ns.Name).ClusterQueue(prodClusterQ.Name).Obj()
		gomega.Expect(k8sClient.Create(ctx, prodLocalQ)).Should(gomega.Succeed())

		ginkgo.By("job should be unsuspended")
		gomega.Eventually(func() *bool {
			gomega.Expect(k8sClient.Get(ctx, lookupKey, createdProdJob)).Should(gomega.Succeed())
			return createdProdJob.Spec.Suspend
		}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
	})

	ginkgo.When("The workload's admission is removed", func() {
		ginkgo.It("Should restore the original node selectors", func() {
			localQueue := testing.MakeLocalQueue("local-queue", ns.Name).ClusterQueue(prodClusterQ.Name).Obj()
			job := testingjob.MakeJob(jobName, ns.Name).Queue(localQueue.Name).Request(corev1.ResourceCPU, "2").Obj()
			lookupKey := types.NamespacedName{Name: job.Name, Namespace: job.Namespace}
			createdJob := &batchv1.Job{}

			ginkgo.By("create a job", func() {
				gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())
			})

			ginkgo.By("job should be suspend", func() {
				gomega.Eventually(func() *bool {
					gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())
					return createdJob.Spec.Suspend
				}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
			})

			// backup the the podSet's node selector
			originalNodeSelector := createdJob.Spec.Template.Spec.NodeSelector

			ginkgo.By("create a localQueue", func() {
				gomega.Expect(k8sClient.Create(ctx, localQueue)).Should(gomega.Succeed())
			})

			ginkgo.By("job should be unsuspended", func() {
				gomega.Eventually(func() *bool {
					gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())
					return createdJob.Spec.Suspend
				}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
			})

			ginkgo.By("the node selector should be updated", func() {
				gomega.Eventually(func() map[string]string {
					gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())
					return createdJob.Spec.Template.Spec.NodeSelector
				}, util.Timeout, util.Interval).ShouldNot(gomega.Equal(originalNodeSelector))
			})

			ginkgo.By("delete the localQueue to prevent readmission", func() {
				gomega.Expect(util.DeleteLocalQueue(ctx, k8sClient, localQueue)).Should(gomega.Succeed())
			})

			ginkgo.By("clear the workload's admission to stop the job", func() {
				wl := &kueue.Workload{}
				wlKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(job.Name), Namespace: job.Namespace}
				gomega.Expect(k8sClient.Get(ctx, wlKey, wl)).Should(gomega.Succeed())
				gomega.Expect(util.SetAdmission(ctx, k8sClient, wl, nil)).Should(gomega.Succeed())
			})

			ginkgo.By("the node selector should be restored", func() {
				gomega.Eventually(func() map[string]string {
					gomega.Expect(k8sClient.Get(ctx, lookupKey, createdJob)).Should(gomega.Succeed())
					return createdJob.Spec.Template.Spec.NodeSelector
				}, util.Timeout, util.Interval).Should(gomega.Equal(originalNodeSelector))
			})
		})
	})

	ginkgo.It("Should allow reclaim of resources that are no longer needed", func() {
		ginkgo.By("creating localQueue", func() {
			prodLocalQ = testing.MakeLocalQueue("prod-queue", ns.Name).ClusterQueue(prodClusterQ.Name).Obj()
			gomega.Expect(k8sClient.Create(ctx, prodLocalQ)).Should(gomega.Succeed())
		})

		job1 := testingjob.MakeJob("job1", ns.Name).Queue(prodLocalQ.Name).
			Request(corev1.ResourceCPU, "2").
			Completions(5).
			Parallelism(2).
			Obj()
		lookupKey1 := types.NamespacedName{Name: job1.Name, Namespace: job1.Namespace}

		ginkgo.By("checking the first job starts", func() {
			gomega.Expect(k8sClient.Create(ctx, job1)).Should(gomega.Succeed())
			createdJob1 := &batchv1.Job{}
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, lookupKey1, createdJob1)).Should(gomega.Succeed())
				return createdJob1.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
			gomega.Expect(createdJob1.Spec.Template.Spec.NodeSelector[instanceKey]).Should(gomega.Equal(onDemandFlavor.Name))
			util.ExpectPendingWorkloadsMetric(prodClusterQ, 0, 0)
			util.ExpectAdmittedActiveWorkloadsMetric(prodClusterQ, 1)
		})

		job2 := testingjob.MakeJob("job2", ns.Name).Queue(prodLocalQ.Name).Request(corev1.ResourceCPU, "3").Obj()
		lookupKey2 := types.NamespacedName{Name: job2.Name, Namespace: job2.Namespace}

		ginkgo.By("checking a second no-fit job does not start", func() {
			gomega.Expect(k8sClient.Create(ctx, job2)).Should(gomega.Succeed())
			createdJob2 := &batchv1.Job{}
			gomega.Consistently(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, lookupKey2, createdJob2)).Should(gomega.Succeed())
				return createdJob2.Spec.Suspend
			}, util.ConsistentDuration, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
			util.ExpectPendingWorkloadsMetric(prodClusterQ, 0, 1)
			util.ExpectAdmittedActiveWorkloadsMetric(prodClusterQ, 1)
		})

		ginkgo.By("checking the second job starts when the first has less then to completions to go", func() {
			createdJob1 := &batchv1.Job{}
			gomega.Expect(k8sClient.Get(ctx, lookupKey1, createdJob1)).Should(gomega.Succeed())
			createdJob1.Status.Succeeded = 4
			gomega.Expect(k8sClient.Status().Update(ctx, createdJob1)).Should(gomega.Succeed())

			wl := &kueue.Workload{}
			wlKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(job1.Name), Namespace: job1.Namespace}
			gomega.Eventually(func() []kueue.ReclaimablePod {
				gomega.Expect(k8sClient.Get(ctx, wlKey, wl)).Should(gomega.Succeed())
				return wl.Status.ReclaimablePods

			}, util.Timeout, util.Interval).Should(gomega.BeComparableTo([]kueue.ReclaimablePod{{
				Name:  "main",
				Count: 1,
			}}))

			createdJob2 := &batchv1.Job{}
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, lookupKey2, createdJob2)).Should(gomega.Succeed())
				return createdJob2.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
			gomega.Expect(createdJob2.Spec.Template.Spec.NodeSelector[instanceKey]).Should(gomega.Equal(onDemandFlavor.Name))

			util.ExpectPendingWorkloadsMetric(prodClusterQ, 0, 0)
			util.ExpectAdmittedActiveWorkloadsMetric(prodClusterQ, 2)
		})
	})

	ginkgo.It("Should readmit preempted Job in alternative flavor", func() {
		devLocalQ = testing.MakeLocalQueue("dev-queue", ns.Name).ClusterQueue(devClusterQ.Name).Obj()
		gomega.Expect(k8sClient.Create(ctx, devLocalQ)).Should(gomega.Succeed())

		highPriorityClass := testing.MakePriorityClass("high").PriorityValue(100).Obj()
		gomega.Expect(k8sClient.Create(ctx, highPriorityClass))
		ginkgo.DeferCleanup(func() {
			gomega.Expect(k8sClient.Delete(ctx, highPriorityClass)).To(gomega.Succeed())
		})

		lowJobKey := types.NamespacedName{Name: "low", Namespace: ns.Name}
		ginkgo.By("Low priority job is unsuspended and has nodeSelector", func() {
			job := testingjob.MakeJob("low", ns.Name).
				Queue(devLocalQ.Name).
				Parallelism(5).
				Request(corev1.ResourceCPU, "1").
				Obj()
			gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())

			expectJobUnsuspendedWithNodeSelectors(lowJobKey, map[string]string{
				instanceKey: "spot-untainted",
			})
		})

		ginkgo.By("High priority job preemtps low priority job", func() {
			job := testingjob.MakeJob("high", ns.Name).
				Queue(devLocalQ.Name).
				PriorityClass("high").
				Parallelism(5).
				Request(corev1.ResourceCPU, "1").
				NodeSelector(instanceKey, "spot-untainted"). // target the same flavor to cause preemption
				Obj()
			gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())

			highJobKey := types.NamespacedName{Name: "high", Namespace: ns.Name}
			expectJobUnsuspendedWithNodeSelectors(highJobKey, map[string]string{
				instanceKey: "spot-untainted",
			})
		})

		ginkgo.By("Preempted job should be admitted on second flavor", func() {
			expectJobUnsuspendedWithNodeSelectors(lowJobKey, map[string]string{
				instanceKey: "on-demand",
			})
		})
	})

	ginkgo.It("Should schedule jobs when partial admission is enabled", func() {
		origPartialAdmission := features.Enabled(features.PartialAdmission)
		ginkgo.By("enable partial admission", func() {
			gomega.Expect(features.SetEnable(features.PartialAdmission, true)).To(gomega.Succeed())
		})

		prodLocalQ = testing.MakeLocalQueue("prod-queue", ns.Name).ClusterQueue(prodClusterQ.Name).Obj()
		job1 := testingjob.MakeJob("job1", ns.Name).
			Queue(prodLocalQ.Name).
			Parallelism(5).
			Completions(6).
			Request(corev1.ResourceCPU, "2").
			Obj()
		jobKey := types.NamespacedName{Name: job1.Name, Namespace: job1.Namespace}
		wlKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(job1.Name), Namespace: job1.Namespace}

		ginkgo.By("creating localQueues")
		gomega.Expect(k8sClient.Create(ctx, prodLocalQ)).Should(gomega.Succeed())

		ginkgo.By("creating the job")
		gomega.Expect(k8sClient.Create(ctx, job1)).Should(gomega.Succeed())

		createdJob := &batchv1.Job{}
		ginkgo.By("the job should stay suspended", func() {
			gomega.Consistently(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
				return createdJob.Spec.Suspend
			}, util.ConsistentDuration, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
		})

		ginkgo.By("enable partial admission", func() {
			gomega.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
			if createdJob.Annotations == nil {
				createdJob.Annotations = map[string]string{
					workloadjob.JobMinParallelismAnnotation: "1",
				}
			} else {
				createdJob.Annotations[workloadjob.JobMinParallelismAnnotation] = "1"
			}

			gomega.Expect(k8sClient.Update(ctx, createdJob)).Should(gomega.Succeed())
		})

		wl := &kueue.Workload{}
		ginkgo.By("the job should be unsuspended with a lower parallelism", func() {
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
				return createdJob.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(false)))
			gomega.Expect(*createdJob.Spec.Parallelism).To(gomega.BeEquivalentTo(2))

			gomega.Expect(k8sClient.Get(ctx, wlKey, wl)).To(gomega.Succeed())
			gomega.Expect(wl.Spec.PodSets[0].MinCount).ToNot(gomega.BeNil())
			gomega.Expect(*wl.Spec.PodSets[0].MinCount).To(gomega.BeEquivalentTo(1))
		})

		ginkgo.By("delete the localQueue to prevent readmission", func() {
			gomega.Expect(util.DeleteLocalQueue(ctx, k8sClient, prodLocalQ)).Should(gomega.Succeed())
		})

		ginkgo.By("clear the workloads admission to stop the job", func() {
			gomega.Expect(util.SetAdmission(ctx, k8sClient, wl, nil)).To(gomega.Succeed())
		})

		ginkgo.By("job should be suspended and its parallelism restored", func() {
			gomega.Eventually(func() *bool {
				gomega.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
				return createdJob.Spec.Suspend
			}, util.Timeout, util.Interval).Should(gomega.Equal(pointer.Bool(true)))
			gomega.Expect(*createdJob.Spec.Parallelism).To(gomega.BeEquivalentTo(5))
		})

		ginkgo.By("restore partial admission", func() {
			gomega.Expect(features.SetEnable(features.PartialAdmission, origPartialAdmission)).To(gomega.Succeed())
		})
	})

	ginkgo.It("Should set the flavor's node selectors if the job is admitted by pods count only", func() {
		localQ := testing.MakeLocalQueue("dev-queue", ns.Name).ClusterQueue(podsCountClusterQ.Name).Obj()
		gomega.Expect(k8sClient.Create(ctx, localQ)).Should(gomega.Succeed())
		ginkgo.By("Creating a job with no requests, will set the resource flavors selectors when admitted ", func() {
			job := testingjob.MakeJob("job", ns.Name).
				Queue(localQ.Name).
				Parallelism(2).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, job)).Should(gomega.Succeed())
			expectJobUnsuspendedWithNodeSelectors(client.ObjectKeyFromObject(job), map[string]string{
				instanceKey: "on-demand",
			})
		})
	})
})

func expectJobUnsuspendedWithNodeSelectors(key types.NamespacedName, ns map[string]string) {
	job := &batchv1.Job{}
	gomega.EventuallyWithOffset(1, func() []any {
		gomega.Expect(k8sClient.Get(ctx, key, job)).To(gomega.Succeed())
		return []any{*job.Spec.Suspend, job.Spec.Template.Spec.NodeSelector}
	}, util.Timeout, util.Interval).Should(gomega.Equal([]any{false, ns}))
}
