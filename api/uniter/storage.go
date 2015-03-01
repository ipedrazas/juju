// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package uniter

import (
	"github.com/juju/errors"
	"github.com/juju/names"

	"github.com/juju/juju/api/base"
	"github.com/juju/juju/api/watcher"
	"github.com/juju/juju/apiserver/params"
)

type StorageAccessor struct {
	facade base.FacadeCaller
}

// NewStorageAccessor creates a StorageAccessor on the specified facade,
// and uses this name when calling through the caller.
func NewStorageAccessor(facade base.FacadeCaller) *StorageAccessor {
	return &StorageAccessor{facade}
}

// UnitStorageAttachments returns the storage instances attached to a unit.
func (sa *StorageAccessor) UnitStorageAttachments(unitTag names.UnitTag) ([]params.StorageAttachment, error) {
	if sa.facade.BestAPIVersion() < 2 {
		// UnitStorageAttachments() was introduced in UniterAPIV2.
		return nil, errors.NotImplementedf("UnitStorageAttachments() (need V2+)")
	}
	args := params.Entities{
		Entities: []params.Entity{
			{Tag: unitTag.String()},
		},
	}
	var results params.StorageAttachmentsResults
	err := sa.facade.FacadeCall("UnitStorageAttachments", args, &results)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(results.Results) != 1 {
		panic(errors.Errorf("expected 1 result, got %d", len(results.Results)))
	}
	result := results.Results[0]
	if result.Error != nil {
		return nil, result.Error
	}
	return result.Result, nil
}

// WatchUnitStorageAttachments starts a watcher for changes to storage
// attachments related to the unit. The watcher will return the
// IDs of the corresponding storage instances.
func (sa *StorageAccessor) WatchUnitStorageAttachments(unitTag names.UnitTag) (watcher.StringsWatcher, error) {
	var results params.StringsWatchResults
	args := params.Entities{
		Entities: []params.Entity{{Tag: unitTag.String()}},
	}
	err := sa.facade.FacadeCall("WatchUnitStorageAttachments", args, &results)
	if err != nil {
		return nil, err
	}
	if len(results.Results) != 1 {
		return nil, errors.Errorf("expected 1 result, got %d", len(results.Results))
	}
	result := results.Results[0]
	if result.Error != nil {
		return nil, result.Error
	}
	w := watcher.NewStringsWatcher(sa.facade.RawAPICaller(), result)
	return w, nil
}

// StorageAttachment returns the storage attachment with the specified
// unit and storage tags.
func (sa *StorageAccessor) StorageAttachment(storageTag names.StorageTag, unitTag names.UnitTag) (params.StorageAttachment, error) {
	if sa.facade.BestAPIVersion() < 2 {
		// StorageAttachment() was introduced in UniterAPIV2.
		return params.StorageAttachment{}, errors.NotImplementedf("StorageAttachment() (need V2+)")
	}
	args := params.StorageAttachmentIds{
		Ids: []params.StorageAttachmentId{{
			StorageTag: storageTag.String(),
			UnitTag:    unitTag.String(),
		}},
	}
	var results params.StorageAttachmentResults
	err := sa.facade.FacadeCall("StorageAttachments", args, &results)
	if err != nil {
		return params.StorageAttachment{}, errors.Trace(err)
	}
	if len(results.Results) != 1 {
		panic(errors.Errorf("expected 1 result, got %d", len(results.Results)))
	}
	result := results.Results[0]
	if result.Error != nil {
		return params.StorageAttachment{}, result.Error
	}
	return result.Result, nil
}

// WatchStorageAttachments start a watcher for changes to the storage
// attachment with the specified unit and storage tags.
func (sa *StorageAccessor) WatchStorageAttachment(storageTag names.StorageTag, unitTag names.UnitTag) (watcher.NotifyWatcher, error) {
	var results params.NotifyWatchResults
	args := params.StorageAttachmentIds{
		Ids: []params.StorageAttachmentId{{
			StorageTag: storageTag.String(),
			UnitTag:    unitTag.String(),
		}},
	}
	err := sa.facade.FacadeCall("WatchStorageAttachments", args, &results)
	if err != nil {
		return nil, err
	}
	if len(results.Results) != 1 {
		return nil, errors.Errorf("expected 1 result, got %d", len(results.Results))
	}
	result := results.Results[0]
	if result.Error != nil {
		return nil, result.Error
	}
	w := watcher.NewNotifyWatcher(sa.facade.RawAPICaller(), result)
	return w, nil
}