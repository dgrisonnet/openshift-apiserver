package imagestreamtag

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	etcd "go.etcd.io/etcd/client/v3"
	authorizationapi "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/user"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	etcdtesting "k8s.io/apiserver/pkg/storage/etcd3/testing"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	kapi "k8s.io/kubernetes/pkg/apis/core"

	imagev1 "github.com/openshift/api/image/v1"
	imageapi "github.com/openshift/openshift-apiserver/pkg/image/apis/image"
	"github.com/openshift/openshift-apiserver/pkg/image/apis/image/validation/fake"
	admfake "github.com/openshift/openshift-apiserver/pkg/image/apiserver/admission/fake"
	"github.com/openshift/openshift-apiserver/pkg/image/apiserver/registry/image"
	imageetcd "github.com/openshift/openshift-apiserver/pkg/image/apiserver/registry/image/etcd"
	"github.com/openshift/openshift-apiserver/pkg/image/apiserver/registry/imagestream"
	imagestreametcd "github.com/openshift/openshift-apiserver/pkg/image/apiserver/registry/imagestream/etcd"
	"github.com/openshift/openshift-apiserver/pkg/image/apiserver/registryhostname"

	_ "github.com/openshift/openshift-apiserver/pkg/api/install"
)

var testDefaultRegistry = func(_ context.Context) (string, bool) { return "defaultregistry:5000", true }

type fakeSubjectAccessReviewRegistry struct {
}

func (f *fakeSubjectAccessReviewRegistry) Create(_ context.Context, subjectAccessReview *authorizationapi.SubjectAccessReview, _ metav1.CreateOptions) (*authorizationapi.SubjectAccessReview, error) {
	return nil, nil
}

func (f *fakeSubjectAccessReviewRegistry) CreateContext(ctx context.Context, subjectAccessReview *authorizationapi.SubjectAccessReview) (*authorizationapi.SubjectAccessReview, error) {
	return nil, nil
}

type fakeUser struct {
}

var _ user.Info = &fakeUser{}

func (u *fakeUser) GetName() string {
	return "user"
}

func (u *fakeUser) GetUID() string {
	return "uid"
}

func (u *fakeUser) GetGroups() []string {
	return []string{"group1"}
}

func (u *fakeUser) GetExtra() map[string][]string {
	return map[string][]string{}
}

func setup(t *testing.T, imagestreamRegistryBuilder func(s imagestream.Storage, status, internal rest.Updater) imagestream.Registry) (etcd.KV, *etcdtesting.EtcdTestServer, *REST) {
	server, etcdStorage := etcdtesting.NewUnsecuredEtcd3TestClientServer(t)
	etcdStorage.Codec = legacyscheme.Codecs.LegacyCodec(schema.GroupVersion{Group: "image.openshift.io", Version: "v1"})
	etcdClient := etcd.NewKV(server.V3Client)
	etcdStorageConfigForImageStreams := &storagebackend.ConfigForResource{Config: *etcdStorage, GroupResource: schema.GroupResource{Group: "image.openshift.io", Resource: "imagestreams"}}
	imagestreamRESTOptions := generic.RESTOptions{StorageConfig: etcdStorageConfigForImageStreams, Decorator: generic.UndecoratedStorage, DeleteCollectionWorkers: 1, ResourcePrefix: "imagestreams"}
	rw := &fake.RegistryWhitelister{}

	etcdStorageConfigForImages := &storagebackend.ConfigForResource{Config: *etcdStorage, GroupResource: schema.GroupResource{Group: "image.openshift.io", Resource: "images"}}
	imageRESTOptions := generic.RESTOptions{StorageConfig: etcdStorageConfigForImages, Decorator: generic.UndecoratedStorage, DeleteCollectionWorkers: 1, ResourcePrefix: "images"}
	imageStorage, err := imageetcd.NewREST(imageRESTOptions)
	if err != nil {
		t.Fatal(err)
	}
	registry := registryhostname.TestingRegistryHostnameRetriever(testDefaultRegistry, "", "")
	imageStreamStorage, _, imageStreamStatus, internalStorage, err := imagestreametcd.NewRESTWithLimitVerifier(
		imagestreamRESTOptions,
		registry,
		&fakeSubjectAccessReviewRegistry{},
		&admfake.ImageStreamLimitVerifier{},
		rw,
		imagestreametcd.NewEmptyLayerIndex(),
	)
	if err != nil {
		t.Fatal(err)
	}

	imageRegistry := image.NewRegistry(imageStorage)
	var imageStreamRegistry imagestream.Registry = nil

	if imagestreamRegistryBuilder == nil {
		imageStreamRegistry = imagestream.NewRegistry(imageStreamStorage, imageStreamStatus, internalStorage)
	} else {
		imageStreamRegistry = imagestreamRegistryBuilder(imageStreamStorage, imageStreamStatus, internalStorage)
	}
	storage := NewREST(imageRegistry, imageStreamRegistry, rw)

	return etcdClient, server, storage
}

type statusError interface {
	Status() metav1.Status
}

func TestGetImageStreamTag(t *testing.T) {
	tests := map[string]struct {
		image           *imageapi.Image
		repo            *imageapi.ImageStream
		expectError     bool
		errorTargetKind string
		errorTargetID   string
	}{
		"happy path": {
			image: &imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
			repo: &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test",
				},
				Spec: imageapi.ImageStreamSpec{
					Tags: map[string]imageapi.TagReference{
						"latest": {
							Annotations: map[string]string{
								"color": "blue",
								"size":  "large",
							},
						},
					},
				},
				Status: imageapi.ImageStreamStatus{
					Tags: map[string]imageapi.TagEventList{
						"latest": {
							Items: []imageapi.TagEvent{
								{
									Created:              metav1.Date(2015, 3, 24, 9, 38, 0, 0, time.UTC),
									DockerImageReference: "test",
									Image:                "10",
								},
							},
						},
					},
				},
			},
		},
		"image = ''": {
			repo: &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: imageapi.ImageStreamStatus{
					Tags: map[string]imageapi.TagEventList{
						"latest": {Items: []imageapi.TagEvent{{DockerImageReference: "test", Image: ""}}},
					},
				}},
			expectError:     true,
			errorTargetKind: "imagestreamtags",
			errorTargetID:   "test:latest",
		},
		"missing image": {
			repo: &imageapi.ImageStream{Status: imageapi.ImageStreamStatus{
				Tags: map[string]imageapi.TagEventList{
					"latest": {Items: []imageapi.TagEvent{{DockerImageReference: "test", Image: "10"}}},
				},
			}},
			expectError:     true,
			errorTargetKind: "images",
			errorTargetID:   "10",
		},
		"missing repo": {
			expectError:     true,
			errorTargetKind: "imagestreams",
			errorTargetID:   "test",
		},
		"missing tag": {
			image: &imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
			repo: &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: imageapi.ImageStreamStatus{
					Tags: map[string]imageapi.TagEventList{
						"other": {Items: []imageapi.TagEvent{{DockerImageReference: "test", Image: "10"}}},
					},
				}},
			expectError:     true,
			errorTargetKind: "imagestreamtags",
			errorTargetID:   "test:latest",
		},
	}

	for name, testCase := range tests {
		func() {
			client, server, storage := setup(t, nil)
			defer server.Terminate(t)

			if testCase.image != nil {
				client.Put(
					context.TODO(),
					etcdtesting.AddPrefix("/images/"+testCase.image.Name),
					runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion), testCase.image),
				)
			}
			if testCase.repo != nil {
				client.Put(
					context.TODO(),
					etcdtesting.AddPrefix("/imagestreams/default/test"),
					runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion), testCase.repo),
				)
			}

			obj, err := storage.Get(apirequest.NewDefaultContext(), "test:latest", &metav1.GetOptions{})
			gotErr := err != nil
			if e, a := testCase.expectError, gotErr; e != a {
				t.Errorf("%s: Expected err=%v: got %v: %v", name, e, a, err)
				return
			}
			if testCase.expectError {
				if !errors.IsNotFound(err) {
					t.Errorf("%s: unexpected error type: %v", name, err)
					return
				}
				status := err.(statusError).Status()
				if status.Details.Kind != testCase.errorTargetKind || status.Details.Name != testCase.errorTargetID {
					t.Errorf("%s: unexpected status: %#v", name, status.Details)
					return
				}
			} else {
				actual := obj.(*imageapi.ImageStreamTag)
				if e, a := "default", actual.Namespace; e != a {
					t.Errorf("%s: namespace: expected %v, got %v", name, e, a)
				}
				if e, a := "test:latest", actual.Name; e != a {
					t.Errorf("%s: name: expected %v, got %v", name, e, a)
				}
				if e, a := map[string]string{"size": "large", "color": "blue"}, actual.Image.Annotations; !reflect.DeepEqual(e, a) {
					t.Errorf("%s: annotations: expected %v, got %v", name, e, a)
				}
				if e, a := metav1.Date(2015, 3, 24, 9, 38, 0, 0, time.UTC), actual.CreationTimestamp; !a.Equal(&e) {
					t.Errorf("%s: timestamp: expected %v, got %v", name, e, a)
				}
			}
		}()
	}
}

func TestGetImageStreamTagDIR(t *testing.T) {
	expDockerImageReference := "foo/bar/baz:latest"
	image := &imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz:different"}
	repo := &imageapi.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
		Status: imageapi.ImageStreamStatus{
			Tags: map[string]imageapi.TagEventList{
				"latest": {
					Items: []imageapi.TagEvent{
						{
							Created:              metav1.Date(2015, 3, 24, 9, 38, 0, 0, time.UTC),
							DockerImageReference: expDockerImageReference,
							Image:                "10",
						},
					},
				},
			},
		},
	}

	client, server, storage := setup(t, nil)
	defer server.Terminate(t)
	client.Put(
		context.TODO(),
		etcdtesting.AddPrefix("/images/"+image.Name),
		runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion), image),
	)
	client.Put(
		context.TODO(),
		etcdtesting.AddPrefix("/imagestreams/default/test"),
		runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion), repo),
	)
	obj, err := storage.Get(apirequest.NewDefaultContext(), "test:latest", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	actual := obj.(*imageapi.ImageStreamTag)
	if actual.Image.DockerImageReference != expDockerImageReference {
		t.Errorf("Different DockerImageReference: expected %s, got %s", expDockerImageReference, actual.Image.DockerImageReference)
	}
}

func TestDeleteImageStreamTag(t *testing.T) {
	tests := map[string]struct {
		repo        *imageapi.ImageStream
		expectError bool
	}{
		"repo not found": {
			expectError: true,
		},
		"nil tag map": {
			repo: &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test",
				},
			},
			expectError: true,
		},
		"missing tag": {
			repo: &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test",
				},
				Spec: imageapi.ImageStreamSpec{
					Tags: map[string]imageapi.TagReference{
						"other": {
							From: &kapi.ObjectReference{
								Kind: "ImageStreamTag",
								Name: "test:foo",
							},
						},
					},
				},
			},
			expectError: true,
		},
		"happy path": {
			repo: &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:  "default",
					Name:       "test",
					Generation: 2,
				},
				Spec: imageapi.ImageStreamSpec{
					Tags: map[string]imageapi.TagReference{
						"another": {
							From: &kapi.ObjectReference{
								Kind: "ImageStreamTag",
								Name: "test:foo",
							},
						},
						"latest": {
							From: &kapi.ObjectReference{
								Kind: "ImageStreamTag",
								Name: "test:bar",
							},
						},
					},
				},
				Status: imageapi.ImageStreamStatus{
					DockerImageRepository: "registry.default.local/default/test",
					Tags: map[string]imageapi.TagEventList{
						"another": {
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Generation:           2,
								},
							},
						},
						"foo": {
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Generation:           2,
								},
							},
						},
						"latest": {
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Generation:           2,
								},
							},
						},
						"bar": {
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
									Generation:           2,
								},
							},
						},
					},
				},
			},
		},
	}

	for name, testCase := range tests {
		func() {
			client, server, storage := setup(t, nil)
			defer server.Terminate(t)

			if testCase.repo != nil {
				client.Put(
					context.TODO(),
					etcdtesting.AddPrefix("/imagestreams/default/test"),
					runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion), testCase.repo),
				)
			}

			ctx := apirequest.WithUser(apirequest.NewDefaultContext(), &fakeUser{})
			obj, _, err := storage.Delete(ctx, "test:latest", nil, nil)
			gotError := err != nil
			if e, a := testCase.expectError, gotError; e != a {
				t.Fatalf("%s: expectError=%t, gotError=%t: %s", name, e, a, err)
			}
			if testCase.expectError {
				return
			}

			if obj == nil {
				t.Fatalf("%s: unexpected nil response", name)
			}
			expectedStatus := &metav1.Status{Status: metav1.StatusSuccess}
			if e, a := expectedStatus, obj; !reflect.DeepEqual(e, a) {
				t.Errorf("%s:\nexpect=%#v\nactual=%#v", name, e, a)
			}

			updatedRepo, err := storage.imageStreamRegistry.GetImageStream(apirequest.NewDefaultContext(), "test", &metav1.GetOptions{})
			if err != nil {
				t.Fatalf("%s: error retrieving updated repo: %s", name, err)
			}
			three := int64(3)
			expectedStreamSpec := map[string]imageapi.TagReference{
				"another": {
					Name: "another",
					From: &kapi.ObjectReference{
						Kind: "ImageStreamTag",
						Name: "test:foo",
					},
					Generation: &three,
					ReferencePolicy: imageapi.TagReferencePolicy{
						Type: imageapi.SourceTagReferencePolicy,
					},
				},
			}
			expectedStreamStatus := map[string]imageapi.TagEventList{
				"another": {
					Items: []imageapi.TagEvent{
						{
							DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
							Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
							Generation:           2,
						},
					},
				},
				"foo": {
					Items: []imageapi.TagEvent{
						{
							DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
							Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
							Generation:           2,
						},
					},
				},
				"bar": {
					Items: []imageapi.TagEvent{
						{
							DockerImageReference: "registry.default.local/default/test@sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
							Image:                "sha256:381151ac5b7f775e8371e489f3479b84a4c004c90ceddb2ad80b6877215a892f",
							Generation:           2,
						},
					},
				},
			}

			if updatedRepo.Generation != 3 {
				t.Errorf("%s: unexpected generation: %d", name, updatedRepo.Generation)
			}
			if e, a := expectedStreamStatus, updatedRepo.Status.Tags; !reflect.DeepEqual(e, a) {
				t.Errorf("%s: stream spec:\nexpect=%#v\nactual=%#v", name, e, a)
			}
			if e, a := expectedStreamSpec, updatedRepo.Spec.Tags; !reflect.DeepEqual(e, a) {
				t.Errorf("%s: stream spec:\nexpect=%#v\nactual=%#v", name, e, a)
			}
		}()
	}
}

func TestCreateImageStreamTag(t *testing.T) {
	tests := map[string]struct {
		istag           runtime.Object
		expectError     bool
		errorTargetKind string
		errorTargetID   string
	}{
		"valid istag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
		},
		"invalid tag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag:   &imageapi.TagReference{},
			},
			expectError:     true,
			errorTargetKind: "ImageStreamTag",
			errorTargetID:   "test:tag",
		},
		"nil tag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
			},
		},
	}

	for name, tc := range tests {
		func() {
			client, server, storage := setup(t, nil)
			defer server.Terminate(t)

			client.Put(
				context.TODO(),
				etcdtesting.AddPrefix("/imagestreams/default/test"),
				runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion),
					&imageapi.ImageStream{
						ObjectMeta: metav1.ObjectMeta{
							CreationTimestamp: metav1.Date(2015, 3, 24, 9, 38, 0, 0, time.UTC),
							Namespace:         "default",
							Name:              "test",
						},
						Spec: imageapi.ImageStreamSpec{
							Tags: map[string]imageapi.TagReference{},
						},
					},
				))

			ctx := apirequest.WithUser(apirequest.NewDefaultContext(), &fakeUser{})
			_, err := storage.Create(ctx, tc.istag, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
			gotErr := err != nil
			if e, a := tc.expectError, gotErr; e != a {
				t.Errorf("%s: Expected err=%v: got %v: %v", name, e, a, err)
				return
			}
			if tc.expectError {
				status := err.(statusError).Status()
				if status.Details.Kind != tc.errorTargetKind || status.Details.Name != tc.errorTargetID {
					t.Errorf("%s: unexpected status: %#v", name, status.Details)
					return
				}
			}
		}()
	}
}

func TestUpdateImageStreamTag(t *testing.T) {
	tests := map[string]struct {
		istag                     runtime.Object
		expectError               bool
		createUpdateConflictError bool
		createUpdateInvalidError  bool
		createCreateConflictError bool
		createCreateInvalidError  bool
		errorTargetKind           string
		errorTargetID             string
		expectCreate              bool
		expectNilResult           bool
		suppressPut               bool
	}{
		"valid istag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectError:     false,
			expectNilResult: false,
			expectCreate:    true,
		},
		"valid istag invalid error": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectError:              true,
			expectNilResult:          true,
			expectCreate:             false,
			createUpdateInvalidError: true,
		},
		"valid istag conflict error": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectError:               false,
			expectNilResult:           false,
			expectCreate:              true,
			createUpdateConflictError: true,
		},
		"valid istag conflict error create": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectError:               true,
			expectNilResult:           true,
			expectCreate:              false,
			createCreateConflictError: true,
			suppressPut:               true,
		},
		"valid istag invalid error create": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectError:              true,
			expectNilResult:          true,
			expectCreate:             false,
			createCreateInvalidError: true,
			suppressPut:              true,
		},
		"invalid tag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag:   &imageapi.TagReference{},
			},
			expectError:     true,
			errorTargetKind: "ImageStreamTag",
			errorTargetID:   "test:tag",
			expectNilResult: true,
		},
		"nil tag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
			},
			expectNilResult: true,
			expectError:     true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {

			client, server, storage := setup(t,

				func(s imagestream.Storage, status, internal rest.Updater) imagestream.Registry {

					apiTesters := make(map[string]*ApiTester)

					if tc.createUpdateConflictError || tc.createUpdateInvalidError {

						apiTester := NewApiTester()
						updateResponses := make(map[int32]ApiResponse)
						apiResponse := NewApiResponse()

						if tc.createUpdateConflictError {

							gr := schema.GroupResource{Group: "imageregistry.operator.openshift.io", Resource: "configs"}
							errors.NewConflict(gr, "test", fmt.Errorf("testing error"))

							apiResponse.response["error"] = errors.NewConflict(gr, "test", fmt.Errorf("testing error"))
							updateResponses[0] = apiResponse
						} else if tc.createUpdateInvalidError {

							gk := schema.GroupKind{Group: "imageregistry.operator.openshift.io", Kind: "anyKind"}
							apiResponse.response["error"] = errors.NewInvalid(gk, "test", nil)
							updateResponses[0] = apiResponse
						}

						apiTester.callResponses = updateResponses

						apiTesters["UpdateImageStream"] = apiTester
					}

					if tc.createCreateConflictError || tc.createCreateInvalidError {

						apiTester := NewApiTester()
						updateResponses := make(map[int32]ApiResponse)
						apiResponse := NewApiResponse()

						if tc.createCreateConflictError {

							gr := schema.GroupResource{Group: "imageregistry.operator.openshift.io", Resource: "configs"}
							errors.NewConflict(gr, "test", fmt.Errorf("testing error"))

							apiResponse.response["error"] = errors.NewConflict(gr, "test", fmt.Errorf("testing error"))
							updateResponses[0] = apiResponse
						} else if tc.createCreateInvalidError {

							gk := schema.GroupKind{Group: "imageregistry.operator.openshift.io", Kind: "anyKind"}
							apiResponse.response["error"] = errors.NewInvalid(gk, "test", nil)
							updateResponses[0] = apiResponse
						}

						apiTester.callResponses = updateResponses

						apiTesters["CreateImageStream"] = apiTester
					}

					return NewImageStreamRegistryTester(imagestream.NewRegistry(s, status, internal), apiTesters)
				},
			)

			defer server.Terminate(t)

			if !tc.suppressPut {
				client.Put(
					context.TODO(),
					etcdtesting.AddPrefix("/imagestreams/default/test"),
					runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion),
						&imageapi.ImageStream{
							ObjectMeta: metav1.ObjectMeta{
								CreationTimestamp: metav1.Date(2015, 3, 24, 9, 38, 0, 0, time.UTC),
								Namespace:         "default",
								Name:              "test",
							},
							Spec: imageapi.ImageStreamSpec{
								Tags: map[string]imageapi.TagReference{},
							},
						},
					))
			}

			ctx := apirequest.WithUser(apirequest.NewDefaultContext(), &fakeUser{})
			istag, ok := tc.istag.(*imageapi.ImageStreamTag)

			if !ok {
				t.Errorf("%s: obj is not an ImageStreamTag: %#v", name, tc.istag)
				return
			}

			result, create, err := storage.Update(ctx, istag.Name, rest.DefaultUpdatedObjectInfo(istag), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})

			gotErr := err != nil
			if e, a := tc.expectError, gotErr; e != a {
				t.Errorf("%s: Expected err=%v: got %v: %v", name, e, a, err)
				return
			}
			if tc.expectError {

				if !gotErr {
					t.Errorf("%s: Expected err but did not receive one", name)
					return
				}

				status := err.(statusError).Status()
				if tc.errorTargetKind != "" {

					if nil == status.Details {
						t.Errorf("%s: Invalid status details, expected: %s got nil", name, tc.errorTargetKind)
						return
					}

					if status.Details.Kind != tc.errorTargetKind || status.Details.Name != tc.errorTargetID {
						t.Errorf("%s: unexpected status: %#v", name, status.Details)
						return
					}

				}

			}

			if result == nil && !tc.expectNilResult {
				t.Errorf("%s: Invalid result (nil)", name)
				return
			}

			if create != tc.expectCreate {
				t.Errorf("%s: Invalid create value: %t", name, create)
				return
			}

			if nil != result {
				resultTag, resultOk := result.(*imageapi.ImageStreamTag)

				if !resultOk {
					t.Errorf("%s: result is not an ImageStreamTag: %#v", name, result)
					return
				}

				if resultTag.ObjectMeta.Name != istag.Name {
					t.Errorf("%s: result contains unexpected ImageStreamTag name: %s, expected %s", name, resultTag.Tag.Name, istag.Name)
					return
				}
			}

		})
	}
}

func TestUpdateRetryImageStreamTag(t *testing.T) {
	tests := map[string]struct {
		istag                     runtime.Object
		expectError               bool
		createUpdateConflictError bool
		createUpdateInvalidError  bool
		createCreateConflictError bool
		createCreateInvalidError  bool
		errorTargetKind           string
		errorTargetID             string
		expectCreate              bool
		expectRetry               bool
		expectNilResult           bool
		suppressPut               bool
	}{
		"valid istag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectRetry:     false,
			expectError:     false,
			expectNilResult: false,
			expectCreate:    true,
		},
		"valid istag invalid error": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectRetry:              true,
			expectError:              true,
			expectNilResult:          true,
			expectCreate:             false,
			createUpdateInvalidError: true,
		},
		"valid istag conflict error": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectRetry:               true,
			expectError:               true,
			expectNilResult:           true,
			expectCreate:              false,
			createUpdateConflictError: true,
		},
		"valid istag conflict error create": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectRetry:               false,
			expectError:               true,
			expectNilResult:           true,
			expectCreate:              false,
			createCreateConflictError: true,
			suppressPut:               true,
		},
		"valid istag invalid error create": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag: &imageapi.TagReference{
					Name:            "latest",
					From:            &kapi.ObjectReference{Kind: "DockerImage", Name: "foo/bar/baz"},
					ReferencePolicy: imageapi.TagReferencePolicy{Type: imageapi.SourceTagReferencePolicy},
				},
			},
			expectRetry:              false,
			expectError:              true,
			expectNilResult:          true,
			expectCreate:             false,
			createCreateInvalidError: true,
			suppressPut:              true,
		},
		"invalid tag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
				Tag:   &imageapi.TagReference{},
			},
			expectError:     true,
			expectRetry:     false,
			expectNilResult: true,
		},
		"nil tag": {
			istag: &imageapi.ImageStreamTag{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test:tag",
				},
				Image: imageapi.Image{ObjectMeta: metav1.ObjectMeta{Name: "10"}, DockerImageReference: "foo/bar/baz"},
			},
			expectNilResult: true,
			expectError:     true,
			expectRetry:     false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client, server, storage := setup(t,

				func(s imagestream.Storage, status, internal rest.Updater) imagestream.Registry {

					apiTesters := make(map[string]*ApiTester)

					if tc.createUpdateConflictError || tc.createUpdateInvalidError {

						apiTester := NewApiTester()
						updateResponses := make(map[int32]ApiResponse)
						apiResponse := NewApiResponse()

						if tc.createUpdateConflictError {

							gr := schema.GroupResource{Group: "imageregistry.operator.openshift.io", Resource: "configs"}
							errors.NewConflict(gr, "test", fmt.Errorf("testing error"))

							apiResponse.response["error"] = errors.NewConflict(gr, "test", fmt.Errorf("testing error"))
							updateResponses[0] = apiResponse
						} else if tc.createUpdateInvalidError {

							gk := schema.GroupKind{Group: "imageregistry.operator.openshift.io", Kind: "anyKind"}
							apiResponse.response["error"] = errors.NewInvalid(gk, "test", nil)
							updateResponses[0] = apiResponse
						}

						apiTester.callResponses = updateResponses

						apiTesters["UpdateImageStream"] = apiTester
					}

					if tc.createCreateConflictError || tc.createCreateInvalidError {

						apiTester := NewApiTester()
						updateResponses := make(map[int32]ApiResponse)
						apiResponse := NewApiResponse()

						if tc.createCreateConflictError {

							gr := schema.GroupResource{Group: "imageregistry.operator.openshift.io", Resource: "configs"}
							errors.NewConflict(gr, "test", fmt.Errorf("testing error"))

							apiResponse.response["error"] = errors.NewConflict(gr, "test", fmt.Errorf("testing error"))
							updateResponses[0] = apiResponse
						} else if tc.createCreateInvalidError {

							gk := schema.GroupKind{Group: "imageregistry.operator.openshift.io", Kind: "anyKind"}
							apiResponse.response["error"] = errors.NewInvalid(gk, "test", nil)
							updateResponses[0] = apiResponse
						}

						apiTester.callResponses = updateResponses

						apiTesters["CreateImageStream"] = apiTester
					}

					return NewImageStreamRegistryTester(imagestream.NewRegistry(s, status, internal), apiTesters)
				},
			)

			defer server.Terminate(t)

			if !tc.suppressPut {
				client.Put(
					context.TODO(),
					etcdtesting.AddPrefix("/imagestreams/default/test"),
					runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(imagev1.SchemeGroupVersion),
						&imageapi.ImageStream{
							ObjectMeta: metav1.ObjectMeta{
								CreationTimestamp: metav1.Date(2015, 3, 24, 9, 38, 0, 0, time.UTC),
								Namespace:         "default",
								Name:              "test",
							},
							Spec: imageapi.ImageStreamSpec{
								Tags: map[string]imageapi.TagReference{},
							},
						},
					))
			}

			ctx := apirequest.WithUser(apirequest.NewDefaultContext(), &fakeUser{})
			istag, ok := tc.istag.(*imageapi.ImageStreamTag)

			if !ok {
				t.Errorf("%s: obj is not an ImageStreamTag: %#v", name, tc.istag)
				return
			}

			result, create, canRetry, err := storage.update(ctx, istag.Name, rest.DefaultUpdatedObjectInfo(istag), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})

			gotErr := err != nil
			if e, a := tc.expectError, gotErr; e != a {
				t.Errorf("%s: Expected err=%v: got %v: %v", name, e, a, err)
				return
			}
			if tc.expectError {

				if !gotErr {
					t.Errorf("%s: Expected err but did not receive one", name)
					return
				}

				status := err.(statusError).Status()
				if tc.errorTargetKind != "" {

					if nil == status.Details {
						t.Errorf("%s: Invalid status details, expected: %s got nil", name, tc.errorTargetKind)
						return
					}

					if status.Details.Kind != tc.errorTargetKind || status.Details.Name != tc.errorTargetID {
						t.Errorf("%s: unexpected status: %#v", name, status.Details)
						return
					}

				}

			}

			if result == nil && !tc.expectNilResult {
				t.Errorf("%s: Invalid result (nil)", name)
				return
			}

			if create != tc.expectCreate {
				t.Errorf("%s: Invalid create value: %t", name, create)
				return
			}

			if canRetry != tc.expectRetry {
				t.Errorf("%s: Invalid retry value: %t", name, canRetry)
				return
			}

			if nil != result {
				resultTag, resultOk := result.(*imageapi.ImageStreamTag)

				if !resultOk {
					t.Errorf("%s: result is not an ImageStreamTag: %#v", name, result)
					return
				}

				if resultTag.ObjectMeta.Name != istag.Name {
					t.Errorf("%s: result contains unexpected ImageStreamTag name: %s, expected %s", name, resultTag.Tag.Name, istag.Name)
					return
				}
			}

		})
	}
}
