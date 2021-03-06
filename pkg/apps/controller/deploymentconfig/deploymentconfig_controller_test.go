package deploymentconfig

import (
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	kinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	kapihelper "k8s.io/kubernetes/pkg/apis/core/helper"

	appsapi "github.com/openshift/origin/pkg/apps/apis/apps"
	_ "github.com/openshift/origin/pkg/apps/apis/apps/install"
	appstest "github.com/openshift/origin/pkg/apps/apis/apps/test"
	appsfake "github.com/openshift/origin/pkg/apps/generated/internalclientset/fake"
	"github.com/openshift/origin/pkg/apps/generated/listers/apps/internalversion"
	appsutil "github.com/openshift/origin/pkg/apps/util"
)

func alwaysReady() bool { return true }

func TestHandleScenarios(t *testing.T) {
	type deployment struct {
		// version is the deployment version
		version int64
		// replicas is the spec replicas of the deployment
		replicas int32
		// test is whether this is a test deployment config
		test bool
		// replicasA is the annotated replica value for backwards compat checks
		replicasA *int32
		desiredA  *int32
		status    appsapi.DeploymentStatus
		cancelled bool
	}

	mkdeployment := func(d deployment) *v1.ReplicationController {
		config := appstest.OkDeploymentConfig(d.version)
		if d.test {
			config = appstest.TestDeploymentConfig(config)
		}
		config.Namespace = "test"
		deployment, _ := appsutil.MakeDeploymentV1(config)
		deployment.Annotations[appsapi.DeploymentStatusAnnotation] = string(d.status)
		if d.cancelled {
			deployment.Annotations[appsapi.DeploymentCancelledAnnotation] = appsapi.DeploymentCancelledAnnotationValue
			deployment.Annotations[appsapi.DeploymentStatusReasonAnnotation] = appsapi.DeploymentCancelledNewerDeploymentExists
		}
		if d.desiredA != nil {
			deployment.Annotations[appsapi.DesiredReplicasAnnotation] = strconv.Itoa(int(*d.desiredA))
		} else {
			delete(deployment.Annotations, appsapi.DesiredReplicasAnnotation)
		}
		deployment.Spec.Replicas = &d.replicas
		return deployment
	}

	tests := []struct {
		name string
		// replicas is the config replicas prior to the update
		replicas int32
		// test is whether this is a test deployment config
		test bool
		// newVersion is the version of the config at the time of the update
		newVersion int64
		// expectedReplicas is the expected config replica count after the update
		expectedReplicas int32
		// before is the state of all deployments prior to the update
		before []deployment
		// after is the expected state of all deployments after the update
		after []deployment
		// errExpected is whether the update should produce an error
		errExpected bool
	}{
		{
			name:             "version is zero",
			replicas:         1,
			newVersion:       0,
			expectedReplicas: 1,
			before:           []deployment{},
			after:            []deployment{},
			errExpected:      false,
		},
		{
			name:             "first deployment",
			replicas:         1,
			newVersion:       1,
			expectedReplicas: 1,
			before:           []deployment{},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "initial deployment already in progress",
			replicas:         1,
			newVersion:       1,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusNew, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "new version",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "already in progress",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusNew, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "already deployed",
			replicas:         1,
			newVersion:       1,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "awaiting cancellation of older deployments",
			replicas:         1,
			newVersion:       3,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), desiredA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusRunning, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), desiredA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusRunning, cancelled: true},
			},
			errExpected: true,
		},
		{
			name:             "awaiting cancellation of older deployments (already cancelled)",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusRunning, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusRunning, cancelled: true},
			},
			errExpected: true,
		},
		{
			name:             "steady state replica corrections (latest == active)",
			replicas:         1,
			newVersion:       5,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
				{version: 5, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
				{version: 5, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "steady state replica corrections (latest != active)",
			replicas:         1,
			newVersion:       5,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 5, replicas: 1, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 5, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "already deployed, no active deployment",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "scale up latest/active completed deployment",
			replicas:         5,
			newVersion:       2,
			expectedReplicas: 5,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 5, replicasA: newInt32(5), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "scale up active (not latest) completed deployment",
			replicas:         5,
			newVersion:       2,
			expectedReplicas: 5,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 5, replicasA: newInt32(5), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "scale down latest/active completed deployment",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 5, replicasA: newInt32(5), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(0), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "scale down active (not latest) completed deployment",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 5, replicasA: newInt32(5), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "fallback to last completed deployment",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newInt32(1), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(1), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "fallback to last completed deployment (partial rollout)",
			replicas:         5,
			newVersion:       2,
			expectedReplicas: 5,
			before: []deployment{
				{version: 1, replicas: 2, replicasA: newInt32(5), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 2, replicasA: newInt32(0), desiredA: newInt32(5), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 5, replicasA: newInt32(5), status: appsapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newInt32(0), desiredA: newInt32(5), status: appsapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
	}

	for _, test := range tests {
		t.Logf("evaluating test: %s", test.name)

		var updatedConfig *appsapi.DeploymentConfig
		deployments := map[string]*v1.ReplicationController{}
		toStore := []*v1.ReplicationController{}
		for _, template := range test.before {
			deployment := mkdeployment(template)
			deployments[deployment.Name] = deployment
			toStore = append(toStore, deployment)
		}

		oc := &appsfake.Clientset{}
		oc.AddReactor("update", "deploymentconfigs", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			dc := action.(clientgotesting.UpdateAction).GetObject().(*appsapi.DeploymentConfig)
			updatedConfig = dc
			return true, dc, nil
		})
		kc := &fake.Clientset{}
		kc.AddReactor("create", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			rc := action.(clientgotesting.CreateAction).GetObject().(*v1.ReplicationController)
			deployments[rc.Name] = rc
			return true, rc, nil
		})
		kc.AddReactor("update", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			rc := action.(clientgotesting.UpdateAction).GetObject().(*v1.ReplicationController)
			deployments[rc.Name] = rc
			return true, rc, nil
		})

		dcInformer := &fakeDeploymentConfigInformer{
			informer: cache.NewSharedIndexInformer(
				&cache.ListWatch{
					ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
						return oc.Apps().DeploymentConfigs(metav1.NamespaceAll).List(options)
					},
					WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
						return oc.Apps().DeploymentConfigs(metav1.NamespaceAll).Watch(options)
					},
				},
				&appsapi.DeploymentConfig{},
				2*time.Minute,
				cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			),
		}

		kubeInformerFactory := kinformers.NewSharedInformerFactory(kc, 0)
		rcInformer := kubeInformerFactory.Core().V1().ReplicationControllers()
		c := NewDeploymentConfigController(dcInformer, rcInformer, oc, kc)
		c.dcStoreSynced = alwaysReady
		c.rcListerSynced = alwaysReady

		for i := range toStore {
			rcInformer.Informer().GetStore().Add(toStore[i])
		}

		config := appstest.OkDeploymentConfig(test.newVersion)
		if test.test {
			config = appstest.TestDeploymentConfig(config)
		}
		config.Spec.Replicas = test.replicas
		config.Namespace = "test"

		if err := c.Handle(config); err != nil && !test.errExpected {
			t.Errorf("unexpected error: %s", err)
			continue
		}

		expectedDeployments := []*v1.ReplicationController{}
		for _, template := range test.after {
			expectedDeployments = append(expectedDeployments, mkdeployment(template))
		}
		actualDeployments := []*v1.ReplicationController{}
		for _, deployment := range deployments {
			actualDeployments = append(actualDeployments, deployment)
		}
		sort.Sort(appsutil.ByLatestVersionDescV1(expectedDeployments))
		sort.Sort(appsutil.ByLatestVersionDescV1(actualDeployments))

		if updatedConfig != nil {
			config = updatedConfig
		}

		if e, a := test.expectedReplicas, config.Spec.Replicas; e != a {
			t.Errorf("expected config replicas to be %d, got %d", e, a)
			continue
		}
		for i := 0; i < len(expectedDeployments); i++ {
			expected, actual := expectedDeployments[i], actualDeployments[i]
			if !kapihelper.Semantic.DeepEqual(expected, actual) {
				t.Errorf("actual deployment don't match expected: %v", diff.ObjectDiff(expected, actual))
			}
		}
	}
}

type fakeDeploymentConfigInformer struct {
	informer cache.SharedIndexInformer
}

func (f *fakeDeploymentConfigInformer) Informer() cache.SharedIndexInformer {
	return f.informer
}

func (f *fakeDeploymentConfigInformer) Lister() internalversion.DeploymentConfigLister {
	return internalversion.NewDeploymentConfigLister(f.informer.GetIndexer())
}

func newInt32(i int32) *int32 {
	return &i
}

func newDC(version, replicas, maxUnavailable int, cond appsapi.DeploymentCondition) *appsapi.DeploymentConfig {
	return &appsapi.DeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Generation: 1,
		},
		Spec: appsapi.DeploymentConfigSpec{
			Replicas: int32(replicas),
			Strategy: appsapi.DeploymentStrategy{
				Type: appsapi.DeploymentStrategyTypeRolling,
				RollingParams: &appsapi.RollingDeploymentStrategyParams{
					MaxUnavailable: intstr.FromInt(maxUnavailable),
					MaxSurge:       intstr.FromInt(1),
				},
			},
		},
		Status: appsapi.DeploymentConfigStatus{
			LatestVersion: int64(version),
			Conditions: []appsapi.DeploymentCondition{
				cond,
			},
		},
	}
}

var (
	availableCond = appsapi.DeploymentCondition{
		Type:   appsapi.DeploymentAvailable,
		Status: kapi.ConditionTrue,
	}
	unavailableCond = appsapi.DeploymentCondition{
		Type:   appsapi.DeploymentAvailable,
		Status: kapi.ConditionFalse,
	}
)

func newRC(version, desired, current, ready, available int32) *v1.ReplicationController {
	return &v1.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{appsapi.DeploymentVersionAnnotation: strconv.Itoa(int(version))},
		},
		Spec: v1.ReplicationControllerSpec{
			Replicas: &desired,
		},
		Status: v1.ReplicationControllerStatus{
			Replicas:          current,
			ReadyReplicas:     ready,
			AvailableReplicas: available,
		},
	}
}

func TestCalculateStatus(t *testing.T) {
	tests := []struct {
		name string

		dc                       *appsapi.DeploymentConfig
		rcs                      []*v1.ReplicationController
		updateObservedGeneration bool

		expected appsapi.DeploymentConfigStatus
	}{
		{
			name: "available deployment",

			dc: newDC(3, 3, 1, availableCond),
			rcs: []*v1.ReplicationController{
				newRC(3, 2, 2, 1, 1),
				newRC(2, 0, 0, 0, 0),
				newRC(1, 0, 1, 1, 1),
			},

			expected: appsapi.DeploymentConfigStatus{
				LatestVersion:     int64(3),
				Replicas:          int32(3),
				ReadyReplicas:     int32(2),
				AvailableReplicas: int32(2),
				UpdatedReplicas:   int32(2),
				Conditions: []appsapi.DeploymentCondition{
					availableCond,
				},
			},
		},
		{
			name: "available deployment with updating observedGeneration",

			dc: newDC(3, 3, 1, availableCond),
			rcs: []*v1.ReplicationController{
				newRC(3, 2, 2, 1, 1),
				newRC(2, 0, 0, 0, 0),
				newRC(1, 0, 1, 1, 1),
			},
			updateObservedGeneration: true,

			expected: appsapi.DeploymentConfigStatus{
				LatestVersion:      int64(3),
				ObservedGeneration: int64(1),
				Replicas:           int32(3),
				ReadyReplicas:      int32(2),
				AvailableReplicas:  int32(2),
				UpdatedReplicas:    int32(2),
				Conditions: []appsapi.DeploymentCondition{
					availableCond,
				},
			},
		},
		{
			name: "unavailable deployment",

			dc: newDC(2, 2, 0, unavailableCond),
			rcs: []*v1.ReplicationController{
				newRC(2, 2, 0, 0, 0),
				newRC(1, 0, 1, 1, 1),
			},

			expected: appsapi.DeploymentConfigStatus{
				LatestVersion:       int64(2),
				Replicas:            int32(1),
				ReadyReplicas:       int32(1),
				AvailableReplicas:   int32(1),
				UpdatedReplicas:     int32(0),
				UnavailableReplicas: int32(1),
				Conditions: []appsapi.DeploymentCondition{
					unavailableCond,
				},
			},
		},
	}

	for _, test := range tests {
		status := calculateStatus(test.dc, test.rcs, test.updateObservedGeneration)
		if !reflect.DeepEqual(status, test.expected) {
			t.Errorf("%s: expected status:\n%+v\ngot status:\n%+v", test.name, test.expected, status)
		}
	}
}
