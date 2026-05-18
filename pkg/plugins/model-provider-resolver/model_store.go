/*
Copyright 2026 The opendatahub.io Authors.

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

package model_provider_resolver

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// externalModelInfo holds the provider and secret name/namespace for an external model.
type externalModelInfo struct {
	provider        string
	targetModel     string // this is the name of the model that will be used in the request
	secretName      string
	secretNamespace string
}

// modelInfoStore is a thread-safe in-memory store that maps model names to their provider info.
// The reconciler writes to it; the plugin reads from it during request processing.
type modelInfoStore struct {
	// externalModelToModelInfo maps namespace -> external model name -> externalModelInfo.
	externalModelToModelInfo map[string]map[string]*externalModelInfo

	lock sync.RWMutex
}

func newModelInfoStore() *modelInfoStore {
	return &modelInfoStore{
		externalModelToModelInfo: make(map[string]map[string]*externalModelInfo),
	}
}

// addOrUpdateExternalModel stores ExternalModel information.
func (s *modelInfoStore) addOrUpdateExternalModel(externalModelKey types.NamespacedName, modelInfo *externalModelInfo) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, found := s.externalModelToModelInfo[externalModelKey.Namespace]; !found {
		s.externalModelToModelInfo[externalModelKey.Namespace] = make(map[string]*externalModelInfo)
	}
	s.externalModelToModelInfo[externalModelKey.Namespace][externalModelKey.Name] = modelInfo
}

// deleteExternalModel deletes ExternalModel information.
func (s *modelInfoStore) deleteExternalModel(externalModelKey types.NamespacedName) {
	s.lock.Lock()
	defer s.lock.Unlock()
	modelsByNamespace, found := s.externalModelToModelInfo[externalModelKey.Namespace]
	if !found {
		return
	}
	delete(modelsByNamespace, externalModelKey.Name)
	if len(modelsByNamespace) == 0 {
		delete(s.externalModelToModelInfo, externalModelKey.Namespace)
	}
}

// getModelInfo returns the modelInfo stored in ExternalModel and bool if found or not.
// if no externalModelInfo found, nil is returned in the first return value.
func (s *modelInfoStore) getModelInfo(externalModelKey types.NamespacedName) (*externalModelInfo, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	modelsByNamespace, found := s.externalModelToModelInfo[externalModelKey.Namespace]
	if !found {
		return nil, false // ExternalModel namespace not found
	}

	externalModelInfo, ok := modelsByNamespace[externalModelKey.Name]
	if !ok {
		return nil, false // ExternalModel not found
	}

	return externalModelInfo, true
}
