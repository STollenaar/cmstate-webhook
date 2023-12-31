package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	cachev1alpha1 "github.com/stollenaar/cmstate-injector-operator/api/v1alpha1"
	"k8s.io/api/admission/v1beta1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
	clientSet     *kubernetes.Clientset
)

type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func init() {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientSet, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
}

func handleMutate(w http.ResponseWriter, r *http.Request) {

	// read the body / request
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		err = fmt.Errorf("error reading body from request: %v", err)
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}
	var review v1beta1.AdmissionReview
	if _, _, err := deserializer.Decode([]byte(body), nil, &review); err != nil {
		err = fmt.Errorf("error deserializing request body: %v, with body: %v", err, body)
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}
	var pod *v1.Pod

	ar := review.Request

	var arData []byte
	if ar.Object.Raw != nil {
		arData = ar.Object.Raw
	} else {
		arData = ar.OldObject.Raw
	}

	if ar != nil {
		// get the Pod object and unmarshal it into its struct, if we cannot, we might as well stop here
		if err := json.Unmarshal(arData, &pod); err != nil {
			err = fmt.Errorf("error unmarshalling review request into pod: %v, with review.Request: %v", err, ar)
			fmt.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s", err)
			return
		}
	}

	review.Response = &v1beta1.AdmissionResponse{
		Allowed: true,
		UID:     review.Request.UID,
	}
	cmState := &cachev1alpha1.CMState{}
	var cmStateData, cmTemplateData []byte

	cmTemplate := &cachev1alpha1.CMTemplate{}
	if pod.Annotations["cache.spices.dev/cmtemplate"] != "" {

		crdName := generateName(pod.Annotations["cache.spices.dev/cmtemplate"])

		// get a list of our CRs
		r := clientSet.RESTClient().
			Get().
			AbsPath(
				fmt.Sprintf("/apis/cache.spices.dev/v1alpha1/namespaces/%s/%s",
					pod.Namespace,
					"cmstates",
				),
			).
			Name(crdName)
		cmStateData, err = r.DoRaw(context.TODO())
		if err != nil && !apierrors.IsNotFound(err) {
			err = fmt.Errorf("fetching cmstate has resulted in an error: %v, with response %s", err, string(cmStateData))
			panic(err)
		} else if err == nil {
			if err := json.Unmarshal(cmStateData, cmState); err != nil {
				panic(err)
			}
		}

		// get a list of our CRs
		r = clientSet.RESTClient().
			Get().
			AbsPath(
				fmt.Sprintf("/apis/cache.spices.dev/v1alpha1/%s",
					"cmtemplates",
				),
			).
			Name(pod.Annotations["cache.spices.dev/cmtemplate"])
		cmTemplateData, err = r.DoRaw(context.TODO())
		if err != nil && !apierrors.IsNotFound(err) {
			err = fmt.Errorf("fetching cmtemplate has resulted in an error: %v, with response %s", err, string(cmTemplateData))
			panic(err)
		} else if err == nil {
			if err := json.Unmarshal(cmTemplateData, cmTemplate); err != nil {
				panic(err)
			}
		} else {
			panic(err)
		}
	} else {
		handleResponse(review, w, r)
		return
	}

	fmt.Printf("Ready to handle %s event\n", ar.Operation)
	if ar.Operation == v1beta1.Create {
		review.Response = handlePodCreate(cmState, cmStateData, cmTemplate, pod)
	} else if ar.Operation == v1beta1.Delete {
		review.Response = handlePodDelete(cmState, cmStateData, pod)
	}

	handleResponse(review, w, r)
}

func main() {

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleMutate)

	s := &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	if _, err := os.Stat("/certs/tls.crt"); err == nil {
		fmt.Println("Running in https mode!")
		log.Fatal(s.ListenAndServeTLS("/certs/tls.crt", "/certs/tls.key"))
	} else {
		fmt.Println("Running in http mode!")
		log.Fatal(s.ListenAndServe())
	}
}

func handleResponse(review v1beta1.AdmissionReview, w http.ResponseWriter, r *http.Request) {
	review.Response.UID = review.Request.UID

	resp, err := json.Marshal(review)
	if err != nil {
		err = fmt.Errorf("can't encode response: %v", err)
		fmt.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Printf("Ready to write reponse ...\n")
	if _, err := w.Write(resp); err != nil {
		err = fmt.Errorf("can't write response: %v", err)
		fmt.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func handlePodDelete(cmState *cachev1alpha1.CMState, cmStateData []byte, pod *v1.Pod) *v1beta1.AdmissionResponse {
	response := &v1beta1.AdmissionResponse{
		Allowed: true,
	}
	if cmState.Name == "" {
		return response
	} else if err := json.Unmarshal(cmStateData, cmState); err != nil {
		panic(err)
	}
	podName := pod.GetName()
	if pod.GetGenerateName() != "" {
		podName = pod.GetGenerateName()
	}

	index := findIndex(cmState.Spec.Audience, podName)
	if index == -1 {
		return response
	}
	cmState.Spec.Audience = append(cmState.Spec.Audience[:index], cmState.Spec.Audience[index+1:]...)

	body, _ := json.Marshal(cmState)
	cmStateData, err := clientSet.RESTClient().Patch(types.MergePatchType).
		AbsPath(
			fmt.Sprintf("/apis/cache.spices.dev/v1alpha1/namespaces/%s/%s",
				pod.Namespace,
				"cmstates",
			),
		).
		Body(body).
		Name(cmState.Name).
		DoRaw(context.TODO())
	if err != nil {
		err = fmt.Errorf("patching cmstate has resulted in an error: %v, with response %s", err, string(cmStateData))
		panic(err)
	}

	return response
}

func handlePodCreate(cmState *cachev1alpha1.CMState, cmStateData []byte, cmTemplate *cachev1alpha1.CMTemplate, pod *v1.Pod) *v1beta1.AdmissionResponse {
	response := &v1beta1.AdmissionResponse{
		Allowed: true,
	}

	if cmState.Name == "" {
		// create the cmstate
		cmState = generateCMState(cmTemplate, pod)

		body, _ := json.Marshal(cmState)

		cmStateData, err := clientSet.RESTClient().Post().
			AbsPath(
				fmt.Sprintf("/apis/cache.spices.dev/v1alpha1/namespaces/%s/%s",
					pod.Namespace,
					"cmstates",
				),
			).
			Body(body).
			DoRaw(context.TODO())

		if err != nil {
			err = fmt.Errorf("creating cmstate has resulted in an error: %v, with response %s", err, string(cmStateData))
			panic(err)
		}
	} else if err := json.Unmarshal(cmStateData, cmState); err != nil {
		panic(err)
	}

	patch := []PatchOperation{
		{
			Op:    "add",
			Path:  "/metadata/annotations/vault.hashicorp.com~1agent-configmap",
			Value: cmState.Name,
		},
	}
	pData, _ := json.Marshal(patch)
	response.Patch = pData
	pt := v1beta1.PatchTypeJSONPatch
	response.PatchType = &pt

	return response
}

// Generating a CMState used for later
func generateCMState(cmTemplate *cachev1alpha1.CMTemplate, pod *v1.Pod) *cachev1alpha1.CMState {
	annotations := pod.GetAnnotations()

	labels := make(map[string]string)
	for annotation := range cmTemplate.Spec.Template.AnnotationReplace {
		labels[annotation] = annotations[annotation]
	}

	podName := pod.GetName()
	if podName == "" {
		podName = pod.GetGenerateName()
	}
	return &cachev1alpha1.CMState{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cache.spices.dev/v1alpha1",
			Kind:       "CMState",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateName(cmTemplate.Name),
			Namespace: pod.GetNamespace(),
			Labels:    labels,
		},
		Spec: cachev1alpha1.CMStateSpec{
			Audience: []cachev1alpha1.CMAudience{
				{
					Kind: "Pod",
					Name: podName,
				},
			},
			CMTemplate: cmTemplate.Name,
		},
	}
}

func generateName(cmTemplateName string) string {
	return strings.ToLower(strings.ReplaceAll(fmt.Sprintf("cmstate-%s", cmTemplateName), "_", "-"))
}

func findIndex(slice []cachev1alpha1.CMAudience, name string) int {
	for i, aud := range slice {
		if aud.Name == name {
			return i
		}
	}
	return -1
}
