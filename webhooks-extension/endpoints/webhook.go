/*
Copyright 2019 The Tekton Authors
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

package endpoints

import (
	"encoding/json"
	"errors"
	"fmt"
	logging "github.com/tektoncd/experimental/webhooks-extension/pkg/logging"
	"net/http"
	"strings"

	restful "github.com/emicklei/go-restful"
	eventapi "github.com/knative/eventing-sources/pkg/apis/sources/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r Resource) createWebhook(request *restful.Request, response *restful.Response) {
	logging.Log.Infof("Creating webhook with request: %+v.", request)
	// Install namespace
	installNs := r.Defaults.Namespace
	if installNs == "" {
		installNs = "default"
	}

	webhook := webhook{}
	if err := request.ReadEntity(&webhook); err != nil {
		logging.Log.Errorf("error trying to read request entity as webhook: %s.", err)
		RespondError(response, err, http.StatusBadRequest)
		return
	}

	if webhook.ReleaseName != "" {
		if len(webhook.ReleaseName) > 63 {
			tooLongMessage := fmt.Sprintf("requested release name (%s) must be less than 64 characters", webhook.ReleaseName)
			err := errors.New(tooLongMessage)
			logging.Log.Errorf("error: %s", err.Error())
			RespondError(response, err, http.StatusBadRequest)
			return
		}
	}

	dockerRegDefault := r.Defaults.DockerRegistry
	if webhook.DockerRegistry == "" && dockerRegDefault != "" {
		webhook.DockerRegistry = dockerRegDefault
	}
	logging.Log.Debugf("Docker registry location is: %s", webhook.DockerRegistry)

	namespace := webhook.Namespace
	if namespace == "" {
		err := errors.New("namespace is required, but none was given")
		logging.Log.Errorf("error: %s.", err.Error())
		RespondError(response, err, http.StatusBadRequest)
		return
	}
	logging.Log.Infof("Creating webhook: %v.", webhook)
	pieces := strings.Split(webhook.GitRepositoryURL, "/")
	if len(pieces) < 4 {
		logging.Log.Errorf("error creating webhook: GitRepositoryURL format error (%+v).", webhook.GitRepositoryURL)
		RespondError(response, errors.New("GitRepositoryURL format error"), http.StatusBadRequest)
		return
	}
	apiURL := strings.TrimSuffix(webhook.GitRepositoryURL, pieces[len(pieces)-2]+"/"+pieces[len(pieces)-1]) + "api/v3/"
	ownerRepo := pieces[len(pieces)-2] + "/" + strings.TrimSuffix(pieces[len(pieces)-1], ".git")

	logging.Log.Debugf("Creating GitHub source with apiURL: %s and Owner-repo: %s.", apiURL, ownerRepo)

	entry := eventapi.GitHubSource{
		ObjectMeta: metav1.ObjectMeta{Name: webhook.Name},
		Spec: eventapi.GitHubSourceSpec{
			OwnerAndRepository: ownerRepo,
			EventTypes:         []string{"push", "pull_request"},
			AccessToken: eventapi.SecretValueFromSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "accessToken",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: webhook.AccessTokenRef,
					},
				},
			},
			SecretToken: eventapi.SecretValueFromSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "secretToken",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: webhook.AccessTokenRef,
					},
				},
			},
			Sink: &corev1.ObjectReference{
				APIVersion: "serving.knative.dev/v1alpha1",
				Kind:       "Service",
				Name:       "webhooks-extension-sink",
			},
		},
	}
	if c := strings.Count(apiURL, "."); c == 2 {
		entry.Spec.GitHubAPIURL = apiURL
	} else if c != 1 {
		err := fmt.Errorf("parsing git api url '%s'", apiURL)
		logging.Log.Errorf("Error %s", err.Error())
		RespondError(response, err, http.StatusBadRequest)
		return
	}
	_, err := r.EventSrcClient.SourcesV1alpha1().GitHubSources(installNs).Create(&entry)
	if err != nil {
		logging.Log.Errorf("Error creating GitHub source: %s.", err.Error())
		RespondError(response, err, http.StatusBadRequest)
		return
	}
	webhooks, err := r.readGitHubWebhooks(installNs)
	if err != nil {
		logging.Log.Errorf("error getting GitHub webhooks: %s.", err.Error())
		RespondError(response, err, http.StatusInternalServerError)
		return
	}
	webhooks[webhook.Name] = webhook
	r.writeGitHubWebhooks(installNs, webhooks)
	response.WriteHeader(http.StatusCreated)
}

func (r Resource) getAllWebhooks(request *restful.Request, response *restful.Response) {
	// Install namespace
	installNs := r.Defaults.Namespace
	if installNs == "" {
		installNs = "default"
	}

	logging.Log.Debugf("Get all webhooks in namespace: %s.", installNs)
	sources, err := r.readGitHubWebhooks(installNs)
	if err != nil {
		logging.Log.Errorf("error trying to get webhooks: %s.", err.Error())
		RespondError(response, err, http.StatusInternalServerError)
		return
	}
	sourcesList := []webhook{}
	for _, value := range sources {
		sourcesList = append(sourcesList, value)
	}
	response.WriteEntity(sourcesList)
}

// retrieve retistry secret, helm secret and pipeline name for the github url
func (r Resource) getGitHubWebhook(gitrepourl string, namespace string) (webhook, error) {
	logging.Log.Debugf("Get GitHub webhook in namespace %s with repositoryURL %s.", namespace, gitrepourl)

	sources, err := r.readGitHubWebhooks(namespace)
	if err != nil {
		return webhook{}, err
	}
	for _, source := range sources {
		if source.GitRepositoryURL == gitrepourl {
			return source, nil
		}
	}
	return webhook{}, fmt.Errorf("could not find webhook with GitRepositoryURL: %s", gitrepourl)
}

func (r Resource) readGitHubWebhooks(namespace string) (map[string]webhook, error) {
	logging.Log.Debugf("Reading GitHub webhooks in namespace %s.", namespace)
	configMapClient := r.K8sClient.CoreV1().ConfigMaps(namespace)
	configMap, err := configMapClient.Get(ConfigMapName, metav1.GetOptions{})
	if err != nil {
		logging.Log.Debugf("Creating empty configmap because error getting configmap: %s.", err.Error())
		configMap = &corev1.ConfigMap{}
		configMap.BinaryData = make(map[string][]byte)
	}
	raw, ok := configMap.BinaryData["GitHubSource"]
	var result map[string]webhook
	if ok {
		err = json.Unmarshal(raw, &result)
		if err != nil {
			logging.Log.Errorf("error unmarshalling in readGitHubSource: %s", err.Error())
			return map[string]webhook{}, err
		}
	} else {
		result = make(map[string]webhook)
	}
	logging.Log.Debugf("Found GitHub sources: %v.", result)
	return result, nil
}

func (r Resource) writeGitHubWebhooks(namespace string, sources map[string]webhook) error {
	logging.Log.Debugf("In writeGitHubWebhooks, namespace: %s, webhooks found: %+v", namespace, sources)
	configMapClient := r.K8sClient.CoreV1().ConfigMaps(namespace)
	configMap, err := configMapClient.Get(ConfigMapName, metav1.GetOptions{})
	var create = false
	if err != nil {
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ConfigMapName,
				Namespace: namespace,
			},
		}
		configMap.BinaryData = make(map[string][]byte)
		create = true
	}
	buf, err := json.Marshal(sources)
	if err != nil {
		logging.Log.Errorf("error marshalling GitHub webhooks: %s.", err.Error())
		return err
	}
	configMap.BinaryData["GitHubSource"] = buf
	if create {
		_, err = configMapClient.Create(configMap)
		if err != nil {
			logging.Log.Errorf("error creating configmap for GitHub webhooks: %s.", err.Error())
			return err
		}
	} else {
		_, err = configMapClient.Update(configMap)
		if err != nil {
			logging.Log.Errorf("error updating configmap for GitHub webhooks: %s.", err.Error())
		}
	}
	return nil
}

func (r Resource) getDefaults(request *restful.Request, response *restful.Response) {
	logging.Log.Debugf("getDefaults returning: %v", r.Defaults)
	response.WriteEntity(r.Defaults)
}

// RespondError ...
func RespondError(response *restful.Response, err error, statusCode int) {
	logging.Log.Errorf("Error for RespondError: %s.", err.Error())
	logging.Log.Errorf("Response is %v.", *response)
	response.AddHeader("Content-Type", "text/plain")
	response.WriteError(statusCode, err)
}

// RespondErrorMessage ...
func RespondErrorMessage(response *restful.Response, message string, statusCode int) {
	logging.Log.Errorf("Message for RespondErrorMessage: %s.", message)
	response.AddHeader("Content-Type", "text/plain")
	response.WriteErrorString(statusCode, message)
}

// RespondErrorAndMessage ...
func RespondErrorAndMessage(response *restful.Response, err error, message string, statusCode int) {
	logging.Log.Errorf("Error for RespondErrorAndMessage: %s.", err.Error())
	logging.Log.Errorf("Message for RespondErrorAndMesage: %s.", message)
	response.AddHeader("Content-Type", "text/plain")
	response.WriteErrorString(statusCode, message)
}

// ExtensionWebService returns the webhook webservice
func ExtensionWebService(r Resource) *restful.WebService {
	ws := new(restful.WebService)
	ws.
		Path("/webhooks").
		Consumes(restful.MIME_JSON, restful.MIME_JSON).
		Produces(restful.MIME_JSON, restful.MIME_JSON)

	ws.Route(ws.POST("/").To(r.createWebhook))
	ws.Route(ws.GET("/").To(r.getAllWebhooks))
	ws.Route(ws.GET("/defaults").To(r.getDefaults))

	return ws
}
