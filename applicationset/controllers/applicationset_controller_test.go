package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	crtclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/argoproj/argo-cd/v2/applicationset/generators"
	"github.com/argoproj/argo-cd/v2/applicationset/utils"
	argov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/gitops-engine/pkg/health"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/argoproj/argo-cd/v2/pkg/apis/applicationset/v1alpha1"
	argoprojiov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/applicationset/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned/fake"
	dbmocks "github.com/argoproj/argo-cd/v2/util/db/mocks"
)

type generatorMock struct {
	mock.Mock
}

func (g *generatorMock) GetTemplate(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator) *argoprojiov1alpha1.ApplicationSetTemplate {
	args := g.Called(appSetGenerator)

	return args.Get(0).(*argoprojiov1alpha1.ApplicationSetTemplate)
}

func (g *generatorMock) GenerateParams(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator, _ *argoprojiov1alpha1.ApplicationSet) ([]map[string]interface{}, error) {
	args := g.Called(appSetGenerator)

	return args.Get(0).([]map[string]interface{}), args.Error(1)
}

type rendererMock struct {
	mock.Mock
}

func (g *generatorMock) GetRequeueAfter(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator) time.Duration {
	args := g.Called(appSetGenerator)

	return args.Get(0).(time.Duration)
}

func (r *rendererMock) RenderTemplateParams(tmpl *argov1alpha1.Application, syncPolicy *argoprojiov1alpha1.ApplicationSetSyncPolicy, params map[string]interface{}, useGoTemplate bool) (*argov1alpha1.Application, error) {
	args := r.Called(tmpl, params, useGoTemplate)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*argov1alpha1.Application), args.Error(1)

}

func TestExtractApplications(t *testing.T) {
	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		name                string
		params              []map[string]interface{}
		template            argoprojiov1alpha1.ApplicationSetTemplate
		generateParamsError error
		rendererError       error
		expectErr           bool
		expectedReason      v1alpha1.ApplicationSetReasonType
	}{
		{
			name:   "Generate two applications",
			params: []map[string]interface{}{{"name": "app1"}, {"name": "app2"}},
			template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "name",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			expectedReason: "",
		},
		{
			name:                "Handles error from the generator",
			generateParamsError: fmt.Errorf("error"),
			expectErr:           true,
			expectedReason:      v1alpha1.ApplicationSetReasonApplicationParamsGenerationError,
		},
		{
			name:   "Handles error from the render",
			params: []map[string]interface{}{{"name": "app1"}, {"name": "app2"}},
			template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "name",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			rendererError:  fmt.Errorf("error"),
			expectErr:      true,
			expectedReason: v1alpha1.ApplicationSetReasonRenderTemplateParamsError,
		},
	} {
		cc := c
		app := argov1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test",
			},
		}

		t.Run(cc.name, func(t *testing.T) {

			appSet := &argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(appSet).Build()

			generatorMock := generatorMock{}
			generator := argoprojiov1alpha1.ApplicationSetGenerator{
				List: &argoprojiov1alpha1.ListGenerator{},
			}

			generatorMock.On("GenerateParams", &generator).
				Return(cc.params, cc.generateParamsError)

			generatorMock.On("GetTemplate", &generator).
				Return(&argoprojiov1alpha1.ApplicationSetTemplate{})

			rendererMock := rendererMock{}

			var expectedApps []argov1alpha1.Application

			if cc.generateParamsError == nil {
				for _, p := range cc.params {

					if cc.rendererError != nil {
						rendererMock.On("RenderTemplateParams", getTempApplication(cc.template), p, false).
							Return(nil, cc.rendererError)
					} else {
						rendererMock.On("RenderTemplateParams", getTempApplication(cc.template), p, false).
							Return(&app, nil)
						expectedApps = append(expectedApps, app)
					}
				}
			}

			r := ApplicationSetReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(1),
				Generators: map[string]generators.Generator{
					"List": &generatorMock,
				},
				Renderer:      &rendererMock,
				KubeClientset: kubefake.NewSimpleClientset(),
			}

			got, reason, err := r.generateApplications(argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Generators: []argoprojiov1alpha1.ApplicationSetGenerator{generator},
					Template:   cc.template,
				},
			})

			if cc.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, expectedApps, got)
			assert.Equal(t, cc.expectedReason, reason)
			generatorMock.AssertNumberOfCalls(t, "GenerateParams", 1)

			if cc.generateParamsError == nil {
				rendererMock.AssertNumberOfCalls(t, "RenderTemplateParams", len(cc.params))
			}

		})
	}

}

func TestMergeTemplateApplications(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = argoprojiov1alpha1.AddToScheme(scheme)
	_ = argov1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	for _, c := range []struct {
		name             string
		params           []map[string]interface{}
		template         argoprojiov1alpha1.ApplicationSetTemplate
		overrideTemplate argoprojiov1alpha1.ApplicationSetTemplate
		expectedMerged   argoprojiov1alpha1.ApplicationSetTemplate
		expectedApps     []argov1alpha1.Application
	}{
		{
			name:   "Generate app",
			params: []map[string]interface{}{{"name": "app1"}},
			template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "name",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			overrideTemplate: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:   "test",
					Labels: map[string]string{"foo": "bar"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			expectedMerged: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "test",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value", "foo": "bar"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			expectedApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
						Labels:    map[string]string{"foo": "bar"},
					},
					Spec: argov1alpha1.ApplicationSpec{},
				},
			},
		},
	} {
		cc := c

		t.Run(cc.name, func(t *testing.T) {

			generatorMock := generatorMock{}
			generator := argoprojiov1alpha1.ApplicationSetGenerator{
				List: &argoprojiov1alpha1.ListGenerator{},
			}

			generatorMock.On("GenerateParams", &generator).
				Return(cc.params, nil)

			generatorMock.On("GetTemplate", &generator).
				Return(&cc.overrideTemplate)

			rendererMock := rendererMock{}

			rendererMock.On("RenderTemplateParams", getTempApplication(cc.expectedMerged), cc.params[0], false).
				Return(&cc.expectedApps[0], nil)

			r := ApplicationSetReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(1),
				Generators: map[string]generators.Generator{
					"List": &generatorMock,
				},
				Renderer:      &rendererMock,
				KubeClientset: kubefake.NewSimpleClientset(),
			}

			got, _, _ := r.generateApplications(argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Generators: []argoprojiov1alpha1.ApplicationSetGenerator{generator},
					Template:   cc.template,
				},
			},
			)

			assert.Equal(t, cc.expectedApps, got)
		})
	}

}

func TestCreateOrUpdateInCluster(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// name is human-readable test name
		name string
		// appSet is the ApplicationSet we are generating resources for
		appSet argoprojiov1alpha1.ApplicationSet
		// existingApps are the apps that already exist on the cluster
		existingApps []argov1alpha1.Application
		// desiredApps are the generated apps to create/update
		desiredApps []argov1alpha1.Application
		// expected is what we expect the cluster Applications to look like, after createOrUpdateInCluster
		expected []argov1alpha1.Application
	}{
		{
			name: "Create an app that doesn't exist",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
			},
			existingApps: nil,
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
				},
			},
		},
		{
			name: "Update an existing app with a different project name",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Create a new app and check it doesn't replace the existing app",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app2",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Ensure that labels and annotations are added (via update) into an exiting application",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "app1",
						Labels:      map[string]string{"label-key": "label-value"},
						Annotations: map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Ensure that labels and annotations are removed from an existing app",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Ensure that status and operation fields are not overridden by an update, when removing labels/annotations",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
		},
		{
			name: "Ensure that status and operation fields are not overridden by an update, when removing labels/annotations and adding other fields",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project:     "project",
							Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
							Destination: argov1alpha1.ApplicationDestination{Server: "server", Namespace: "namespace"},
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "app1",
						Labels:      map[string]string{"label-key": "label-value"},
						Annotations: map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project:     "project",
						Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
						Destination: argov1alpha1.ApplicationDestination{Server: "server", Namespace: "namespace"},
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project:     "project",
						Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
						Destination: argov1alpha1.ApplicationDestination{Server: "server", Namespace: "namespace"},
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
		},
		{
			name: "Ensure that argocd notifications state annotation is preserved from an existing app",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations: map[string]string{
							"annot-key":           "annot-value",
							NotifiedAnnotationKey: `{"b620d4600c771a6f4cxxxxxxx:on-deployed:[0].y7b5sbwa2Q329JYHxxxxxx-fBs:slack:slack-test":1617144614}`,
						},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
						Annotations: map[string]string{
							NotifiedAnnotationKey: `{"b620d4600c771a6f4cxxxxxxx:on-deployed:[0].y7b5sbwa2Q329JYHxxxxxx-fBs:slack:slack-test":1617144614}`,
						},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
	} {

		t.Run(c.name, func(t *testing.T) {

			initObjs := []crtclient.Object{&c.appSet}

			for _, a := range c.existingApps {
				err = controllerutil.SetControllerReference(&c.appSet, &a, scheme)
				assert.Nil(t, err)
				initObjs = append(initObjs, &a)
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

			r := ApplicationSetReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(len(initObjs) + len(c.expected)),
			}

			err = r.createOrUpdateInCluster(context.TODO(), c.appSet, c.desiredApps)
			assert.Nil(t, err)

			for _, obj := range c.expected {
				got := &argov1alpha1.Application{}
				_ = client.Get(context.Background(), crtclient.ObjectKey{
					Namespace: obj.Namespace,
					Name:      obj.Name,
				}, got)

				err = controllerutil.SetControllerReference(&c.appSet, &obj, r.Scheme)
				assert.Nil(t, err)
				assert.Equal(t, obj, *got)
			}
		})
	}
}

func TestRemoveFinalizerOnInvalidDestination_FinalizerTypes(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// name is human-readable test name
		name               string
		existingFinalizers []string
		expectedFinalizers []string
	}{
		{
			name:               "no finalizers",
			existingFinalizers: []string{},
			expectedFinalizers: nil,
		},
		{
			name:               "contains only argo finalizer",
			existingFinalizers: []string{argov1alpha1.ResourcesFinalizerName},
			expectedFinalizers: nil,
		},
		{
			name:               "contains only non-argo finalizer",
			existingFinalizers: []string{"non-argo-finalizer"},
			expectedFinalizers: []string{"non-argo-finalizer"},
		},
		{
			name:               "contains both argo and non-argo finalizer",
			existingFinalizers: []string{"non-argo-finalizer", argov1alpha1.ResourcesFinalizerName},
			expectedFinalizers: []string{"non-argo-finalizer"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {

			appSet := argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			}

			app := argov1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "app1",
					Finalizers: c.existingFinalizers,
				},
				Spec: argov1alpha1.ApplicationSpec{
					Project: "project",
					Source:  argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
					// Destination is always invalid, for this test:
					Destination: argov1alpha1.ApplicationDestination{Name: "my-cluster", Namespace: "namespace"},
				},
			}

			initObjs := []crtclient.Object{&app, &appSet}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: "namespace",
					Labels: map[string]string{
						generators.ArgoCDSecretTypeLabel: generators.ArgoCDSecretTypeCluster,
					},
				},
				Data: map[string][]byte{
					// Since this test requires the cluster to be an invalid destination, we
					// always return a cluster named 'my-cluster2' (different from app 'my-cluster', above)
					"name":   []byte("mycluster2"),
					"server": []byte("https://kubernetes.default.svc"),
					"config": []byte("{\"username\":\"foo\",\"password\":\"foo\"}"),
				},
			}

			objects := append([]runtime.Object{}, secret)
			kubeclientset := kubefake.NewSimpleClientset(objects...)

			r := ApplicationSetReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      record.NewFakeRecorder(10),
				KubeClientset: kubeclientset,
			}
			//settingsMgr := settings.NewSettingsManager(context.TODO(), kubeclientset, "namespace")
			//argoDB := db.NewDB("namespace", settingsMgr, r.KubeClientset)
			//clusterList, err := argoDB.ListClusters(context.Background())
			clusterList, err := utils.ListClusters(context.Background(), kubeclientset, "namespace")
			assert.NoError(t, err, "Unexpected error")

			appLog := log.WithFields(log.Fields{"app": app.Name, "appSet": ""})

			appInputParam := app.DeepCopy()

			err = r.removeFinalizerOnInvalidDestination(context.Background(), appSet, appInputParam, clusterList, appLog)
			assert.NoError(t, err, "Unexpected error")

			retrievedApp := argov1alpha1.Application{}
			err = client.Get(context.Background(), crtclient.ObjectKeyFromObject(&app), &retrievedApp)
			assert.NoError(t, err, "Unexpected error")

			// App on the cluster should have the expected finalizers
			assert.ElementsMatch(t, c.expectedFinalizers, retrievedApp.Finalizers)

			// App object passed in as a parameter should have the expected finaliers
			assert.ElementsMatch(t, c.expectedFinalizers, appInputParam.Finalizers)

			bytes, _ := json.MarshalIndent(retrievedApp, "", "  ")
			t.Log("Contents of app after call:", string(bytes))

		})
	}
}

func TestRemoveFinalizerOnInvalidDestination_DestinationTypes(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// name is human-readable test name
		name                   string
		destinationField       argov1alpha1.ApplicationDestination
		expectFinalizerRemoved bool
	}{
		{
			name: "invalid cluster: empty destination",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster: invalid server url",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Server:    "https://1.2.3.4",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster: invalid cluster name",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "invalid-cluster",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster by both valid",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "mycluster2",
				Server:    "https://kubernetes.default.svc",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster by both invalid",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "mycluster3",
				Server:    "https://4.5.6.7",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "valid cluster by name",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "mycluster2",
			},
			expectFinalizerRemoved: false,
		},
		{
			name: "valid cluster by server",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Server:    "https://kubernetes.default.svc",
			},
			expectFinalizerRemoved: false,
		},
	} {

		t.Run(c.name, func(t *testing.T) {

			appSet := argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			}

			app := argov1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "app1",
					Finalizers: []string{argov1alpha1.ResourcesFinalizerName},
				},
				Spec: argov1alpha1.ApplicationSpec{
					Project:     "project",
					Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
					Destination: c.destinationField,
				},
			}

			initObjs := []crtclient.Object{&app, &appSet}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: "namespace",
					Labels: map[string]string{
						generators.ArgoCDSecretTypeLabel: generators.ArgoCDSecretTypeCluster,
					},
				},
				Data: map[string][]byte{
					// Since this test requires the cluster to be an invalid destination, we
					// always return a cluster named 'my-cluster2' (different from app 'my-cluster', above)
					"name":   []byte("mycluster2"),
					"server": []byte("https://kubernetes.default.svc"),
					"config": []byte("{\"username\":\"foo\",\"password\":\"foo\"}"),
				},
			}

			objects := append([]runtime.Object{}, secret)
			kubeclientset := kubefake.NewSimpleClientset(objects...)

			r := ApplicationSetReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      record.NewFakeRecorder(10),
				KubeClientset: kubeclientset,
			}
			// settingsMgr := settings.NewSettingsManager(context.TODO(), kubeclientset, "argocd")
			// argoDB := db.NewDB("argocd", settingsMgr, r.KubeClientset)
			// clusterList, err := argoDB.ListClusters(context.Background())
			clusterList, err := utils.ListClusters(context.Background(), kubeclientset, "namespace")
			assert.NoError(t, err, "Unexpected error")

			appLog := log.WithFields(log.Fields{"app": app.Name, "appSet": ""})

			appInputParam := app.DeepCopy()

			err = r.removeFinalizerOnInvalidDestination(context.Background(), appSet, appInputParam, clusterList, appLog)
			assert.NoError(t, err, "Unexpected error")

			retrievedApp := argov1alpha1.Application{}
			err = client.Get(context.Background(), crtclient.ObjectKeyFromObject(&app), &retrievedApp)
			assert.NoError(t, err, "Unexpected error")

			finalizerRemoved := len(retrievedApp.Finalizers) == 0

			assert.True(t, c.expectFinalizerRemoved == finalizerRemoved)

			bytes, _ := json.MarshalIndent(retrievedApp, "", "  ")
			t.Log("Contents of app after call:", string(bytes))

		})
	}
}

func TestCreateApplications(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		appSet     argoprojiov1alpha1.ApplicationSet
		existsApps []argov1alpha1.Application
		apps       []argov1alpha1.Application
		expected   []argov1alpha1.Application
	}{
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
			},
			existsApps: nil,
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
				},
			},
		},
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existsApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
		},
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existsApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app2",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
	} {
		initObjs := []crtclient.Object{&c.appSet}
		for _, a := range c.existsApps {
			err = controllerutil.SetControllerReference(&c.appSet, &a, scheme)
			assert.Nil(t, err)
			initObjs = append(initObjs, &a)
		}

		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

		r := ApplicationSetReconciler{
			Client:   client,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(len(initObjs) + len(c.expected)),
		}

		err = r.createInCluster(context.TODO(), c.appSet, c.apps)
		assert.Nil(t, err)

		for _, obj := range c.expected {
			got := &argov1alpha1.Application{}
			_ = client.Get(context.Background(), crtclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      obj.Name,
			}, got)

			err = controllerutil.SetControllerReference(&c.appSet, &obj, r.Scheme)
			assert.Nil(t, err)

			assert.Equal(t, obj, *got)
		}
	}

}

func TestDeleteInCluster(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// appSet is the application set on which the delete function is called
		appSet argoprojiov1alpha1.ApplicationSet
		// existingApps is the current state of Applications on the cluster
		existingApps []argov1alpha1.Application
		// desireApps is the apps generated by the generator that we wish to keep alive
		desiredApps []argov1alpha1.Application
		// expected is the list of applications that we expect to exist after calling delete
		expected []argov1alpha1.Application
		// notExpected is the list of applications that we expect not to exist after calling delete
		notExpected []argov1alpha1.Application
	}{
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "delete",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "keep",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "keep",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "keep",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			notExpected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "delete",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
	} {
		initObjs := []crtclient.Object{&c.appSet}
		for _, a := range c.existingApps {
			temp := a
			err = controllerutil.SetControllerReference(&c.appSet, &temp, scheme)
			assert.Nil(t, err)
			initObjs = append(initObjs, &temp)
		}

		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

		r := ApplicationSetReconciler{
			Client:        client,
			Scheme:        scheme,
			Recorder:      record.NewFakeRecorder(len(initObjs) + len(c.expected)),
			KubeClientset: kubefake.NewSimpleClientset(),
		}

		err = r.deleteInCluster(context.TODO(), c.appSet, c.desiredApps)
		assert.Nil(t, err)

		// For each of the expected objects, verify they exist on the cluster
		for _, obj := range c.expected {
			got := &argov1alpha1.Application{}
			_ = client.Get(context.Background(), crtclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      obj.Name,
			}, got)

			err = controllerutil.SetControllerReference(&c.appSet, &obj, r.Scheme)
			assert.Nil(t, err)

			assert.Equal(t, obj, *got)
		}

		// Verify each of the unexpected objs cannot be found
		for _, obj := range c.notExpected {
			got := &argov1alpha1.Application{}
			err := client.Get(context.Background(), crtclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      obj.Name,
			}, got)

			assert.EqualError(t, err, fmt.Sprintf("applications.argoproj.io \"%s\" not found", obj.Name))
		}
	}
}

func TestGetMinRequeueAfter(t *testing.T) {
	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	generator := argoprojiov1alpha1.ApplicationSetGenerator{
		List:     &argoprojiov1alpha1.ListGenerator{},
		Git:      &argoprojiov1alpha1.GitGenerator{},
		Clusters: &argoprojiov1alpha1.ClusterGenerator{},
	}

	generatorMock0 := generatorMock{}
	generatorMock0.On("GetRequeueAfter", &generator).
		Return(generators.NoRequeueAfter)

	generatorMock1 := generatorMock{}
	generatorMock1.On("GetRequeueAfter", &generator).
		Return(time.Duration(1) * time.Second)

	generatorMock10 := generatorMock{}
	generatorMock10.On("GetRequeueAfter", &generator).
		Return(time.Duration(10) * time.Second)

	r := ApplicationSetReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(0),
		Generators: map[string]generators.Generator{
			"List":     &generatorMock10,
			"Git":      &generatorMock1,
			"Clusters": &generatorMock1,
		},
	}

	got := r.getMinRequeueAfter(&argoprojiov1alpha1.ApplicationSet{
		Spec: argoprojiov1alpha1.ApplicationSetSpec{
			Generators: []argoprojiov1alpha1.ApplicationSetGenerator{generator},
		},
	})

	assert.Equal(t, time.Duration(1)*time.Second, got)
}

func TestValidateGeneratedApplications(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	// Valid cluster
	myCluster := argov1alpha1.Cluster{
		Server: "https://kubernetes.default.svc",
		Name:   "my-cluster",
	}

	// Valid project
	myProject := &argov1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "namespace"},
		Spec: argov1alpha1.AppProjectSpec{
			SourceRepos: []string{"*"},
			Destinations: []argov1alpha1.ApplicationDestination{
				{
					Namespace: "*",
					Server:    "*",
				},
			},
			ClusterResourceWhitelist: []metav1.GroupKind{
				{
					Group: "*",
					Kind:  "*",
				},
			},
		},
	}

	// Test a subset of the validations that 'validateGeneratedApplications' performs
	for _, cc := range []struct {
		name             string
		apps             []argov1alpha1.Application
		expectedErrors   []string
		validationErrors map[int]error
	}{
		{
			name: "valid app should return true",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors:   []string{},
			validationErrors: map[int]error{},
		},
		{
			name: "can't have both name and server defined",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Server:    "my-server",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors:   []string{"application destination can't have both name and server defined"},
			validationErrors: map[int]error{0: fmt.Errorf("application destination spec is invalid: application destination can't have both name and server defined: my-cluster my-server")},
		},
		{
			name: "project mismatch should return error",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "DOES-NOT-EXIST",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors:   []string{"application references project DOES-NOT-EXIST which does not exist"},
			validationErrors: map[int]error{0: fmt.Errorf("application references project DOES-NOT-EXIST which does not exist")},
		},
		{
			name: "valid app should return true",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors:   []string{},
			validationErrors: map[int]error{},
		},
		{
			name: "cluster should match",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "nonexistent-cluster",
						},
					},
				},
			},
			expectedErrors:   []string{"there are no clusters with this name: nonexistent-cluster"},
			validationErrors: map[int]error{0: fmt.Errorf("application destination spec is invalid: unable to find destination server: there are no clusters with this name: nonexistent-cluster")},
		},
	} {

		t.Run(cc.name, func(t *testing.T) {

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: "namespace",
					Labels: map[string]string{
						generators.ArgoCDSecretTypeLabel: generators.ArgoCDSecretTypeCluster,
					},
				},
				Data: map[string][]byte{
					"name":   []byte("my-cluster"),
					"server": []byte("https://kubernetes.default.svc"),
					"config": []byte("{\"username\":\"foo\",\"password\":\"foo\"}"),
				},
			}

			objects := append([]runtime.Object{}, secret)
			kubeclientset := kubefake.NewSimpleClientset(objects...)

			argoDBMock := dbmocks.ArgoDB{}
			argoDBMock.On("GetCluster", mock.Anything, "https://kubernetes.default.svc").Return(&myCluster, nil)
			argoDBMock.On("ListClusters", mock.Anything).Return(&argov1alpha1.ClusterList{Items: []argov1alpha1.Cluster{
				myCluster,
			}}, nil)

			argoObjs := []runtime.Object{myProject}
			for _, app := range cc.apps {
				argoObjs = append(argoObjs, &app)
			}

			r := ApplicationSetReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         record.NewFakeRecorder(1),
				Generators:       map[string]generators.Generator{},
				ArgoDB:           &argoDBMock,
				ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
				KubeClientset:    kubeclientset,
			}

			appSetInfo := argoprojiov1alpha1.ApplicationSet{}

			validationErrors, _ := r.validateGeneratedApplications(context.TODO(), cc.apps, appSetInfo, "namespace")
			var errorMessages []string
			for _, v := range validationErrors {
				errorMessages = append(errorMessages, v.Error())
			}

			if len(errorMessages) == 0 {
				assert.Equal(t, len(cc.expectedErrors), 0, "Expected errors but none were seen")
			} else {
				// An error was returned: it should be expected
				matched := false
				for _, expectedErr := range cc.expectedErrors {
					foundMatch := strings.Contains(strings.Join(errorMessages, ";"), expectedErr)
					assert.True(t, foundMatch, "Unble to locate expected error: %s", cc.expectedErrors)
					matched = matched || foundMatch
				}
				assert.True(t, matched, "An unexpected error occurrred: %v", err)
				// validation message was returned: it should be expected
				matched = false
				foundMatch := reflect.DeepEqual(validationErrors, cc.validationErrors)
				var message string
				for _, v := range validationErrors {
					message = v.Error()
					break
				}
				assert.True(t, foundMatch, "Unble to locate validation message: %s", message)
				matched = matched || foundMatch
				assert.True(t, matched, "An unexpected error occurrred: %v", err)
			}
		})
	}
}

func TestReconcilerValidationErrorBehaviour(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	defaultProject := argov1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "argocd"},
		Spec:       argov1alpha1.AppProjectSpec{SourceRepos: []string{"*"}, Destinations: []argov1alpha1.ApplicationDestination{{Namespace: "*", Server: "https://good-cluster"}}},
	}
	appSet := argoprojiov1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "argocd",
		},
		Spec: argoprojiov1alpha1.ApplicationSetSpec{
			GoTemplate: true,
			Generators: []argoprojiov1alpha1.ApplicationSetGenerator{
				{
					List: &argoprojiov1alpha1.ListGenerator{
						Elements: []apiextensionsv1.JSON{{
							Raw: []byte(`{"cluster": "good-cluster","url": "https://good-cluster"}`),
						}, {
							Raw: []byte(`{"cluster": "bad-cluster","url": "https://bad-cluster"}`),
						}},
					},
				},
			},
			Template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "{{.cluster}}",
					Namespace: "argocd",
				},
				Spec: argov1alpha1.ApplicationSpec{
					Source:      argov1alpha1.ApplicationSource{RepoURL: "https://github.com/argoproj/argocd-example-apps", Path: "guestbook"},
					Project:     "default",
					Destination: argov1alpha1.ApplicationDestination{Server: "{{.url}}"},
				},
			},
		},
	}

	kubeclientset := kubefake.NewSimpleClientset()
	argoDBMock := dbmocks.ArgoDB{}
	argoObjs := []runtime.Object{&defaultProject}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&appSet).Build()
	goodCluster := argov1alpha1.Cluster{Server: "https://good-cluster", Name: "good-cluster"}
	badCluster := argov1alpha1.Cluster{Server: "https://bad-cluster", Name: "bad-cluster"}
	argoDBMock.On("GetCluster", mock.Anything, "https://good-cluster").Return(&goodCluster, nil)
	argoDBMock.On("GetCluster", mock.Anything, "https://bad-cluster").Return(&badCluster, nil)
	argoDBMock.On("ListClusters", mock.Anything).Return(&argov1alpha1.ClusterList{Items: []argov1alpha1.Cluster{
		goodCluster,
	}}, nil)

	r := ApplicationSetReconciler{
		Log:      ctrl.Log.WithName("controllers").WithName("ApplicationSet"),
		Client:   client,
		Scheme:   scheme,
		Renderer: &utils.Render{},
		Recorder: record.NewFakeRecorder(1),
		Generators: map[string]generators.Generator{
			"List": generators.NewListGenerator(),
		},
		ArgoDB:           &argoDBMock,
		ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
		KubeClientset:    kubeclientset,
		Policy:           &utils.SyncPolicy{},
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "argocd",
			Name:      "name",
		},
	}

	// Verify that on validation error, no error is returned, but the object is requeued
	res, err := r.Reconcile(context.Background(), req)
	assert.Nil(t, err)
	assert.True(t, res.RequeueAfter == 0)

	var app argov1alpha1.Application

	// make sure good app got created
	err = r.Client.Get(context.TODO(), crtclient.ObjectKey{Namespace: "argocd", Name: "good-cluster"}, &app)
	assert.NoError(t, err)
	assert.Equal(t, app.Name, "good-cluster")

	// make sure bad app was not created
	err = r.Client.Get(context.TODO(), crtclient.ObjectKey{Namespace: "argocd", Name: "bad-cluster"}, &app)
	assert.Error(t, err)
}

func TestSetApplicationSetStatusCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	appSet := argoprojiov1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "argocd",
		},
		Spec: argoprojiov1alpha1.ApplicationSetSpec{
			Generators: []argoprojiov1alpha1.ApplicationSetGenerator{
				{List: &argoprojiov1alpha1.ListGenerator{
					Elements: []apiextensionsv1.JSON{{
						Raw: []byte(`{"cluster": "my-cluster","url": "https://kubernetes.default.svc"}`),
					}},
				}},
			},
			Template: argoprojiov1alpha1.ApplicationSetTemplate{},
		},
	}

	appCondition := argoprojiov1alpha1.ApplicationSetCondition{
		Type:    argoprojiov1alpha1.ApplicationSetConditionResourcesUpToDate,
		Message: "All applications have been generated successfully",
		Reason:  argoprojiov1alpha1.ApplicationSetReasonApplicationSetUpToDate,
		Status:  argoprojiov1alpha1.ApplicationSetConditionStatusTrue,
	}

	kubeclientset := kubefake.NewSimpleClientset([]runtime.Object{}...)
	argoDBMock := dbmocks.ArgoDB{}
	argoObjs := []runtime.Object{}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&appSet).Build()

	r := ApplicationSetReconciler{
		Log:      ctrl.Log.WithName("controllers").WithName("ApplicationSet"),
		Client:   client,
		Scheme:   scheme,
		Renderer: &utils.Render{},
		Recorder: record.NewFakeRecorder(1),
		Generators: map[string]generators.Generator{
			"List": generators.NewListGenerator(),
		},
		ArgoDB:           &argoDBMock,
		ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
		KubeClientset:    kubeclientset,
	}

	err = r.setApplicationSetStatusCondition(context.TODO(), &appSet, appCondition, true)
	assert.Nil(t, err)

	assert.Len(t, appSet.Status.Conditions, 3)
}

func TestSetApplicationSetApplicationStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	appSet := argoprojiov1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "argocd",
		},
		Spec: argoprojiov1alpha1.ApplicationSetSpec{
			Generators: []argoprojiov1alpha1.ApplicationSetGenerator{
				{List: &argoprojiov1alpha1.ListGenerator{
					Elements: []apiextensionsv1.JSON{{
						Raw: []byte(`{"cluster": "my-cluster","url": "https://kubernetes.default.svc"}`),
					}},
				}},
			},
			Template: argoprojiov1alpha1.ApplicationSetTemplate{},
		},
	}

	appStatuses := []argoprojiov1alpha1.ApplicationSetApplicationStatus{
		argoprojiov1alpha1.ApplicationSetApplicationStatus{
			Application:        "my-application",
			LastTransitionTime: &metav1.Time{},
			Message:            "testing SetApplicationSetApplicationStatus to Healthy",
			ObservedGeneration: 1,
			Status:             "Healthy",
		},
	}

	kubeclientset := kubefake.NewSimpleClientset([]runtime.Object{}...)
	argoDBMock := dbmocks.ArgoDB{}
	argoObjs := []runtime.Object{}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&appSet).Build()

	r := ApplicationSetReconciler{
		Log:      ctrl.Log.WithName("controllers").WithName("ApplicationSet"),
		Client:   client,
		Scheme:   scheme,
		Renderer: &utils.Render{},
		Recorder: record.NewFakeRecorder(1),
		Generators: map[string]generators.Generator{
			"List": generators.NewListGenerator(),
		},
		ArgoDB:           &argoDBMock,
		ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
		KubeClientset:    kubeclientset,
	}

	err = r.setApplicationSetApplicationStatus(context.TODO(), &appSet, appStatuses)
	assert.Nil(t, err)

	assert.Len(t, appSet.Status.ApplicationStatus, 1)
}

func TestBuildAppDependencyList(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	for _, cc := range []struct {
		name            string
		appSet          argoprojiov1alpha1.ApplicationSet
		apps            []argov1alpha1.Application
		expectedList    [][]string
		expectedStepMap map[string]int
	}{
		{
			name: "handles an empty set of applications and no strategy",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
			},
			apps:            []argov1alpha1.Application{},
			expectedList:    [][]string{},
			expectedStepMap: map[string]int{},
		},
		{
			name: "handles an empty set of applications and ignores AllAtOnce strategy",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "AllAtOnce",
					},
				},
			},
			apps:            []argov1alpha1.Application{},
			expectedList:    [][]string{},
			expectedStepMap: map[string]int{},
		},
		{
			name: "handles an empty set of applications with good selectors",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"dev",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			apps: []argov1alpha1.Application{},
			expectedList: [][]string{
				{},
			},
			expectedStepMap: map[string]int{},
		},
		{
			name: "handles selecting 1 application with 1 selector",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"dev",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-dev",
						Labels: map[string]string{
							"env": "dev",
						},
					},
				},
			},
			expectedList: [][]string{
				{"app-dev"},
			},
			expectedStepMap: map[string]int{
				"app-dev": 0,
			},
		},
		{
			name: "handles selectors that select no applications",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"dev",
											},
										},
									},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"qa",
											},
										},
									},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"prod",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-qa",
						Labels: map[string]string{
							"env": "qa",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-prod",
						Labels: map[string]string{
							"env": "prod",
						},
					},
				},
			},
			expectedList: [][]string{
				{},
				{"app-qa"},
				{"app-prod"},
			},
			expectedStepMap: map[string]int{
				"app-qa":   1,
				"app-prod": 2,
			},
		},
		{
			name: "handles multiple selectors in the same matchExpression",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "region",
											Operator: "In",
											Values: []string{
												"us-east-2",
											},
										},
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"qa",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-qa1",
						Labels: map[string]string{
							"env": "qa",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-qa2",
						Labels: map[string]string{
							"env":    "qa",
							"region": "us-east-2",
						},
					},
				},
			},
			expectedList: [][]string{
				{"app-qa2"},
			},
			expectedStepMap: map[string]int{
				"app-qa2": 0,
			},
		},
		{
			name: "handles multiple values in the same matchExpression",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{
										{
											Key:      "env",
											Operator: "In",
											Values: []string{
												"qa",
												"prod",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-dev",
						Labels: map[string]string{
							"env": "dev",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-qa",
						Labels: map[string]string{
							"env": "qa",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app-prod",
						Labels: map[string]string{
							"env":    "prod",
							"region": "us-east-2",
						},
					},
				},
			},
			expectedList: [][]string{
				{"app-qa", "app-prod"},
			},
			expectedStepMap: map[string]int{
				"app-qa":   0,
				"app-prod": 0,
			},
		},
	} {

		t.Run(cc.name, func(t *testing.T) {

			kubeclientset := kubefake.NewSimpleClientset([]runtime.Object{}...)
			argoDBMock := dbmocks.ArgoDB{}
			argoObjs := []runtime.Object{}

			r := ApplicationSetReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         record.NewFakeRecorder(1),
				Generators:       map[string]generators.Generator{},
				ArgoDB:           &argoDBMock,
				ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
				KubeClientset:    kubeclientset,
			}

			appDependencyList, appStepMap, err := r.buildAppDependencyList(context.TODO(), cc.appSet, cc.apps)
			assert.Equal(t, err, nil, "expected no errors, but errors occured")
			assert.Equal(t, cc.expectedList, appDependencyList, "expected appDependencyList did not match actual")
			assert.Equal(t, cc.expectedStepMap, appStepMap, "expected appStepMap did not match actual")
		})
	}
}

func TestBuildAppSyncMap(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	for _, cc := range []struct {
		name              string
		appSet            argoprojiov1alpha1.ApplicationSet
		appDependencyList [][]string
		expectedMap       map[string]bool
	}{
		{
			name: "handles an empty app dependency list",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
			},
			appDependencyList: [][]string{},
			expectedMap:       map[string]bool{},
		},
		{
			name: "handles two applications with no statuses",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
			},
			appDependencyList: [][]string{
				{"app1"},
				{"app2"},
			},
			expectedMap: map[string]bool{
				"app1": true,
				"app2": false,
			},
		},
		{
			name: "handles applications after an empty selection",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
			},
			appDependencyList: [][]string{
				{},
				{"app1", "app2"},
			},
			expectedMap: map[string]bool{
				"app1": true,
				"app2": true,
			},
		},
		{
			name: "handles applications that are healthy and match the appset generation",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 1,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							ObservedGeneration: 1,
							Status:             "Healthy",
						},
						{
							Application:        "app2",
							ObservedGeneration: 1,
							Status:             "Healthy",
						},
					},
				},
			},
			appDependencyList: [][]string{
				{"app1"},
				{"app2"},
			},
			expectedMap: map[string]bool{
				"app1": true,
				"app2": true,
			},
		},
		{
			name: "handles applications that are healthy but out of date",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							ObservedGeneration: 1,
							Status:             "Healthy",
						},
						{
							Application:        "app2",
							ObservedGeneration: 1,
							Status:             "Healthy",
						},
					},
				},
			},
			appDependencyList: [][]string{
				{"app1"},
				{"app2"},
			},
			expectedMap: map[string]bool{
				"app1": true,
				"app2": false,
			},
		},
		{
			name: "handles applications that are up to date, but not healthy",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							ObservedGeneration: 2,
							Status:             "Progressing",
						},
						{
							Application:        "app2",
							ObservedGeneration: 2,
							Status:             "Progressing",
						},
					},
				},
			},
			appDependencyList: [][]string{
				{"app1"},
				{"app2"},
			},
			expectedMap: map[string]bool{
				"app1": true,
				"app2": false,
			},
		},
		{
			name: "handles a lot of applications",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							ObservedGeneration: 2,
							Status:             "Healthy",
						},
						{
							Application:        "app2",
							ObservedGeneration: 2,
							Status:             "Healthy",
						},
						{
							Application:        "app4",
							ObservedGeneration: 2,
							Status:             "Healthy",
						},
						{
							Application:        "app7",
							ObservedGeneration: 2,
							Status:             "Healthy",
						},
					},
				},
			},
			appDependencyList: [][]string{
				{"app1", "app4", "app7"},
				{"app2", "app5", "app8"},
				{"app3", "app6", "app9"},
			},
			expectedMap: map[string]bool{
				"app1": true,
				"app2": true,
				"app3": false,
				"app4": true,
				"app5": true,
				"app6": false,
				"app7": true,
				"app8": true,
				"app9": false,
			},
		},
	} {

		t.Run(cc.name, func(t *testing.T) {

			kubeclientset := kubefake.NewSimpleClientset([]runtime.Object{}...)
			argoDBMock := dbmocks.ArgoDB{}
			argoObjs := []runtime.Object{}

			r := ApplicationSetReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         record.NewFakeRecorder(1),
				Generators:       map[string]generators.Generator{},
				ArgoDB:           &argoDBMock,
				ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
				KubeClientset:    kubeclientset,
			}

			appSyncMap, err := r.buildAppSyncMap(context.TODO(), cc.appSet, cc.appDependencyList)
			assert.Equal(t, err, nil, "expected no errors, but errors occured")
			assert.Equal(t, cc.expectedMap, appSyncMap, "expected appSyncMap did not match actual")
		})
	}
}

func TestUpdateApplicationSetApplicationStatus(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, cc := range []struct {
		name              string
		appSet            argoprojiov1alpha1.ApplicationSet
		apps              []argov1alpha1.Application
		expectedAppStatus []argoprojiov1alpha1.ApplicationSetApplicationStatus
	}{
		{
			name: "handles a nil list of statuses and no applications",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				// Status: argoprojiov1alpha1.ApplicationSetStatus{
				// 	ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
				// },
			},
			apps:              []argov1alpha1.Application{},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
		},
		{
			name: "handles a nil list of statuses with a healthy application",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Status: argov1alpha1.ApplicationStatus{
						Health: argov1alpha1.HealthStatus{
							Status: health.HealthStatusHealthy,
						},
					},
				},
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					Message:            "No Application status found, defaulting status to Waiting.",
					ObservedGeneration: 0,
					Status:             "Waiting",
				},
			},
		},
		{
			name: "handles an empty list of statuses with a healthy application",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Status: argov1alpha1.ApplicationStatus{
						Health: argov1alpha1.HealthStatus{
							Status: health.HealthStatusHealthy,
						},
					},
				},
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					Message:            "No Application status found, defaulting status to Waiting.",
					ObservedGeneration: 0,
					Status:             "Waiting",
				},
			},
		},
		{
			name: "progresses an out of date healthy application to waiting",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "",
							ObservedGeneration: 1,
							Status:             "Healthy",
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Status: argov1alpha1.ApplicationStatus{
						Health: argov1alpha1.HealthStatus{
							Status: health.HealthStatusHealthy,
						},
					},
				},
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
					ObservedGeneration: 1,
					Status:             "Waiting",
				},
			},
		},
		{
			name: "progresses a pending application to progressing",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "",
							ObservedGeneration: 2,
							Status:             "Pending",
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Status: argov1alpha1.ApplicationStatus{
						Health: argov1alpha1.HealthStatus{
							Status: health.HealthStatusProgressing,
						},
					},
				},
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					Message:            "Application resource became Progressing, updating status from Pending to Progressing.",
					ObservedGeneration: 2,
					Status:             "Progressing",
				},
			},
		},
		{
			name: "progresses a pending application to healthy on timeout",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							LastTransitionTime: &metav1.Time{},
							Message:            "",
							ObservedGeneration: 2,
							Status:             "Pending",
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Status: argov1alpha1.ApplicationStatus{
						Health: argov1alpha1.HealthStatus{
							Status: health.HealthStatusHealthy,
						},
					},
				},
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					Message:            "Application Pending status timed out while waiting to become Progressing, reset status to Healthy.",
					ObservedGeneration: 2,
					Status:             "Healthy",
				},
			},
		},
		{
			name: "progresses a progressing application to healthy",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "",
							ObservedGeneration: 2,
							Status:             "Progressing",
						},
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Status: argov1alpha1.ApplicationStatus{
						Health: argov1alpha1.HealthStatus{
							Status: health.HealthStatusHealthy,
						},
					},
				},
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					Message:            "Application resource became Healthy, updating status from Progressing to Healthy.",
					ObservedGeneration: 2,
					Status:             "Healthy",
				},
			},
		},
	} {

		t.Run(cc.name, func(t *testing.T) {

			kubeclientset := kubefake.NewSimpleClientset([]runtime.Object{}...)
			argoDBMock := dbmocks.ArgoDB{}
			argoObjs := []runtime.Object{}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&cc.appSet).Build()

			r := ApplicationSetReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         record.NewFakeRecorder(1),
				Generators:       map[string]generators.Generator{},
				ArgoDB:           &argoDBMock,
				ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
				KubeClientset:    kubeclientset,
			}

			appStatuses, err := r.updateApplicationSetApplicationStatus(context.TODO(), &cc.appSet, cc.apps)

			// opt out of testing the LastTransitionTime is accurate
			for i := range appStatuses {
				appStatuses[i].LastTransitionTime = nil
			}

			assert.Equal(t, err, nil, "expected no errors, but errors occured")
			assert.Equal(t, cc.expectedAppStatus, appStatuses, "expected appStatuses did not match actual")
		})
	}
}

func TestUpdateApplicationSetApplicationStatusProgress(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, cc := range []struct {
		name              string
		appSet            argoprojiov1alpha1.ApplicationSet
		appSyncMap        map[string]bool
		appStepMap        map[string]int
		expectedAppStatus []argoprojiov1alpha1.ApplicationSetApplicationStatus
	}{
		{
			name: "handles an empty appSync and appStepMap",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
							},
						},
					},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
				},
			},
			appSyncMap:        map[string]bool{},
			appStepMap:        map[string]int{},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
		},
		{
			name: "handles an empty strategy",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
				},
			},
			appSyncMap:        map[string]bool{},
			appStepMap:        map[string]int{},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
		},
		{
			name: "handles an empty rollingUpdate",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
				},
			},
			appSyncMap:        map[string]bool{},
			appStepMap:        map[string]int{},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
		},
		{
			name: "handles an appSyncMap with no existing statuses",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
				},
			},
			appSyncMap: map[string]bool{
				"app1": true,
				"app2": false,
			},
			appStepMap: map[string]int{
				"app1": 0,
				"app2": 1,
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{},
		},
		{
			name: "handles updating a status from Waiting to Pending",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
							},
						},
					},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
					},
				},
			},
			appSyncMap: map[string]bool{
				"app1": true,
			},
			appStepMap: map[string]int{
				"app1": 0,
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					LastTransitionTime: nil,
					Message:            "Application moved to Pending status, watching for the Application resource to start Progressing.",
					ObservedGeneration: 2,
					Status:             "Pending",
				},
			},
		},
		{
			name: "does not update a status if appSyncMap is false",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
							},
						},
					},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
					},
				},
			},
			appSyncMap: map[string]bool{
				"app1": false,
			},
			appStepMap: map[string]int{
				"app1": 0,
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					LastTransitionTime: nil,
					Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
					ObservedGeneration: 1,
					Status:             "Waiting",
				},
			},
		},
		{
			name: "does not update a status if status is not pending",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
							},
						},
					},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "Application Pending status timed out while waiting to become Progressing, reset status to Healthy.",
							ObservedGeneration: 1,
							Status:             "Healthy",
						},
					},
				},
			},
			appSyncMap: map[string]bool{
				"app1": true,
			},
			appStepMap: map[string]int{
				"app1": 0,
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					LastTransitionTime: nil,
					Message:            "Application Pending status timed out while waiting to become Progressing, reset status to Healthy.",
					ObservedGeneration: 1,
					Status:             "Healthy",
				},
			},
		},
		{
			name: "does not update a status if maxUpdate has already been reached",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
									MaxUpdate: &intstr.IntOrString{
										Type:   intstr.Int,
										IntVal: 3,
									},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
							},
						},
					},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "Application resource became Progressing, updating status from Pending to Progressing.",
							ObservedGeneration: 2,
							Status:             "Progressing",
						},
						{
							Application:        "app2",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
						{
							Application:        "app3",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
						{
							Application:        "app4",
							Message:            "Application moved to Pending status, watching for the Application resource to start Progressing.",
							ObservedGeneration: 2,
							Status:             "Pending",
						},
					},
				},
			},
			appSyncMap: map[string]bool{
				"app1": true,
				"app2": true,
				"app3": true,
				"app4": true,
			},
			appStepMap: map[string]int{
				"app1": 0,
				"app2": 0,
				"app3": 0,
				"app4": 0,
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					LastTransitionTime: nil,
					Message:            "Application resource became Progressing, updating status from Pending to Progressing.",
					ObservedGeneration: 2,
					Status:             "Progressing",
				},
				{
					Application:        "app2",
					LastTransitionTime: nil,
					Message:            "Application moved to Pending status, watching for the Application resource to start Progressing.",
					ObservedGeneration: 2,
					Status:             "Pending",
				},
				{
					Application:        "app3",
					LastTransitionTime: nil,
					Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
					ObservedGeneration: 1,
					Status:             "Waiting",
				},
				{
					Application:        "app4",
					LastTransitionTime: nil,
					Message:            "Application moved to Pending status, watching for the Application resource to start Progressing.",
					ObservedGeneration: 2,
					Status:             "Pending",
				},
			},
		},
		{
			name: "handles maxUpdate set to percentage string",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "name",
					Namespace:  "argocd",
					Generation: 2,
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Strategy: &argoprojiov1alpha1.ApplicationSetStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &argoprojiov1alpha1.ApplicationSetRollingUpdateStrategy{
							Steps: []argoprojiov1alpha1.ApplicationSetRollingUpdateStep{
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
									MaxUpdate: &intstr.IntOrString{
										Type:   intstr.String,
										StrVal: "50%",
									},
								},
								{
									MatchExpressions: []argoprojiov1alpha1.ApplicationMatchExpression{},
								},
							},
						},
					},
				},
				Status: argoprojiov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
						{
							Application:        "app1",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
						{
							Application:        "app2",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
						{
							Application:        "app3",
							Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
							ObservedGeneration: 1,
							Status:             "Waiting",
						},
					},
				},
			},
			appSyncMap: map[string]bool{
				"app1": true,
				"app2": true,
				"app3": true,
			},
			appStepMap: map[string]int{
				"app1": 0,
				"app2": 0,
				"app3": 0,
			},
			expectedAppStatus: []argoprojiov1alpha1.ApplicationSetApplicationStatus{
				{
					Application:        "app1",
					LastTransitionTime: nil,
					Message:            "Application moved to Pending status, watching for the Application resource to start Progressing.",
					ObservedGeneration: 2,
					Status:             "Pending",
				},
				{
					Application:        "app2",
					LastTransitionTime: nil,
					Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
					ObservedGeneration: 1,
					Status:             "Waiting",
				},
				{
					Application:        "app3",
					LastTransitionTime: nil,
					Message:            "Application is out of date with the current AppSet generation, setting status to Waiting.",
					ObservedGeneration: 1,
					Status:             "Waiting",
				},
			},
		},
	} {

		t.Run(cc.name, func(t *testing.T) {

			kubeclientset := kubefake.NewSimpleClientset([]runtime.Object{}...)
			argoDBMock := dbmocks.ArgoDB{}
			argoObjs := []runtime.Object{}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&cc.appSet).Build()

			r := ApplicationSetReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         record.NewFakeRecorder(1),
				Generators:       map[string]generators.Generator{},
				ArgoDB:           &argoDBMock,
				ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
				KubeClientset:    kubeclientset,
			}

			appStatuses, err := r.updateApplicationSetApplicationStatusProgress(context.TODO(), &cc.appSet, cc.appSyncMap, cc.appStepMap)

			// opt out of testing the LastTransitionTime is accurate
			for i := range appStatuses {
				appStatuses[i].LastTransitionTime = nil
			}

			assert.Equal(t, err, nil, "expected no errors, but errors occured")
			assert.Equal(t, cc.expectedAppStatus, appStatuses, "expected appStatuses did not match actual")
		})
	}
}
