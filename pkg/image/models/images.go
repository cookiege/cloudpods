// Copyright 2019 Yunion
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

package models

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/tristate"
	"yunion.io/x/pkg/util/timeutils"
	"yunion.io/x/pkg/utils"
	"yunion.io/x/sqlchemy"

	"yunion.io/x/onecloud/pkg/apis"
	api "yunion.io/x/onecloud/pkg/apis/image"
	"yunion.io/x/onecloud/pkg/appsrv"
	"yunion.io/x/onecloud/pkg/cloudcommon/db"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/quotas"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/image/options"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/mcclient/auth"
	"yunion.io/x/onecloud/pkg/mcclient/modulebase"
	"yunion.io/x/onecloud/pkg/mcclient/modules"
	"yunion.io/x/onecloud/pkg/util/fileutils2"
	"yunion.io/x/onecloud/pkg/util/logclient"
	"yunion.io/x/onecloud/pkg/util/qemuimg"
	"yunion.io/x/onecloud/pkg/util/rbacutils"
	"yunion.io/x/onecloud/pkg/util/streamutils"
	"yunion.io/x/onecloud/pkg/util/stringutils2"
)

const (
	LocalFilePrefix = "file://"
)

type SImageManager struct {
	db.SSharableVirtualResourceBaseManager
}

var ImageManager *SImageManager

var imgStreamingWorkerMan *appsrv.SWorkerManager

func init() {
	ImageManager = &SImageManager{
		SSharableVirtualResourceBaseManager: db.NewSharableVirtualResourceBaseManager(
			SImage{},
			"images",
			"image",
			"images",
		),
	}
	ImageManager.SetVirtualObject(ImageManager)

	imgStreamingWorkerMan = appsrv.NewWorkerManager("image_streaming_worker", 10, 1024, true)
}

/*
+------------------+--------------+------+-----+---------+-------+
| Field            | Type         | Null | Key | Default | Extra |
+------------------+--------------+------+-----+---------+-------+
| id               | varchar(36)  | NO   | PRI | NULL    |       |
| name             | varchar(255) | YES  |     | NULL    |       |
| size             | bigint(20)   | YES  |     | NULL    |       |
| status           | varchar(30)  | NO   |     | NULL    |       |
| is_public        | tinyint(1)   | NO   | MUL | NULL    |       |
| location         | text         | YES  |     | NULL    |       |
| created_at       | datetime     | NO   |     | NULL    |       |
| updated_at       | datetime     | YES  |     | NULL    |       |
| deleted_at       | datetime     | YES  |     | NULL    |       |
| deleted          | tinyint(1)   | NO   | MUL | NULL    |       |
| parent_id        | varchar(36)  | YES  |     | NULL    |       |
| disk_format      | varchar(20)  | YES  |     | NULL    |       |
| container_format | varchar(20)  | YES  |     | NULL    |       |
| checksum         | varchar(32)  | YES  |     | NULL    |       |
| owner            | varchar(255) | YES  |     | NULL    |       |
| min_disk         | int(11)      | NO   |     | NULL    |       |
| min_ram          | int(11)      | NO   |     | NULL    |       |
| protected        | tinyint(1)   | YES  |     | NULL    |       |
| description      | varchar(256) | YES  |     | NULL    |       |
+------------------+--------------+------+-----+---------+-------+
*/
type SImage struct {
	db.SSharableVirtualResourceBase

	// 镜像大小, 单位Byte
	Size int64 `nullable:"true" list:"user" create:"optional"`
	// 存储地址
	Location string `nullable:"true"`

	// 镜像格式
	DiskFormat string `width:"20" charset:"ascii" nullable:"true" list:"user" create:"optional" default:"raw"`
	// 校验和
	Checksum string `width:"32" charset:"ascii" nullable:"true" get:"user" list:"user"`
	FastHash string `width:"32" charset:"ascii" nullable:"true" get:"user"`
	// 用户Id
	Owner string `width:"255" charset:"ascii" nullable:"true" get:"user"`
	// 最小系统盘要求
	MinDiskMB int32 `name:"min_disk" nullable:"false" default:"0" list:"user" create:"optional" update:"user"`
	// 最小内存要求
	MinRamMB int32 `name:"min_ram" nullable:"false" default:"0" list:"user" create:"optional" update:"user"`

	// 是否有删除保护
	Protected tristate.TriState `nullable:"false" default:"true" list:"user" get:"user" create:"optional" update:"user"`
	// 是否是标准镜像
	IsStandard tristate.TriState `nullable:"false" default:"false" list:"user" get:"user" create:"admin_optional"`
	// 是否是主机镜像
	IsGuestImage tristate.TriState `nullable:"false" default:"false" create:"optional" list:"user"`
	// 是否是数据盘镜像
	IsData tristate.TriState `nullable:"false" default:"false" create:"optional" list:"user"`

	// image copy from url, save origin checksum before probe
	// 从镜像时长导入的镜像校验和
	OssChecksum string `width:"32" charset:"ascii" nullable:"true" get:"user" list:"user"`
}

func (manager *SImageManager) CustomizeHandlerInfo(info *appsrv.SHandlerInfo) {
	manager.SVirtualResourceBaseManager.CustomizeHandlerInfo(info)

	switch info.GetName(nil) {
	case "get_details", "create", "update":
		info.SetProcessTimeout(time.Minute * 120).SetWorkerManager(imgStreamingWorkerMan)
	}
}

func (manager *SImageManager) FetchCreateHeaderData(ctx context.Context, header http.Header) (jsonutils.JSONObject, error) {
	return modules.FetchImageMeta(header), nil
}

func (manager *SImageManager) FetchUpdateHeaderData(ctx context.Context, header http.Header) (jsonutils.JSONObject, error) {
	return modules.FetchImageMeta(header), nil
}

func (manager *SImageManager) InitializeData() error {
	// set cloudregion ID
	images := make([]SImage, 0)
	q := manager.Query().IsNullOrEmpty("tenant_id")
	err := db.FetchModelObjects(manager, q, &images)
	if err != nil {
		return err
	}
	for i := 0; i < len(images); i += 1 {
		if len(images[i].ProjectId) == 0 {
			db.Update(&images[i], func() error {
				images[i].ProjectId = images[i].Owner
				return nil
			})
		}
	}
	return nil
}

func (manager *SImageManager) AllowGetPropertyDetail(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) bool {
	return true
}

func (manager *SImageManager) GetPropertyDetail(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	appParams := appsrv.AppContextGetParams(ctx)
	appParams.OverrideResponseBodyWrapper = true

	queryDict := query.(*jsonutils.JSONDict)
	queryDict.Add(jsonutils.JSONTrue, "details")

	items, err := db.ListItems(manager, ctx, userCred, queryDict, nil)
	if err != nil {
		log.Errorf("Fail to list items: %s", err)
		return nil, httperrors.NewGeneralError(err)
	}
	return modulebase.ListResult2JSONWithKey(items, manager.KeywordPlural()), nil
}

func (manager *SImageManager) IsCustomizedGetDetailsBody() bool {
	return true
}

func (self *SImage) CustomizedGetDetailsBody(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	filePath := self.getLocalLocation()
	status := self.Status

	if self.IsGuestImage.IsFalse() {
		formatStr := jsonutils.GetAnyString(query, []string{"format", "disk_format"})
		if len(formatStr) > 0 {
			subimg := ImageSubformatManager.FetchSubImage(self.Id, formatStr)
			if subimg != nil {
				isTorrent := jsonutils.QueryBoolean(query, "torrent", false)
				if !isTorrent {
					filePath = subimg.getLocalLocation()
					status = subimg.Status
				} else {
					filePath = subimg.getLocalTorrentLocation()
					status = subimg.TorrentStatus
				}
			} else {
				return nil, httperrors.NewNotFoundError("format %s not found", formatStr)
			}
		}
	}

	if status != api.IMAGE_STATUS_ACTIVE {
		return nil, httperrors.NewInvalidStatusError("cannot download in status %s", status)
	}

	if filePath == "" {
		return nil, httperrors.NewInvalidStatusError("empty file path")
	}

	appParams := appsrv.AppContextGetParams(ctx)

	fstat, err := os.Stat(filePath)
	if err != nil {
		return nil, errors.Wrap(err, "os.Stat")
	}

	appParams.Response.Header().Set("Content-Length", strconv.FormatInt(fstat.Size(), 10))

	fp, err := os.Open(filePath)
	if err != nil {
		return nil, errors.Wrap(err, "os.Open")
	}
	defer fp.Close()

	_, err = streamutils.StreamPipe(fp, appParams.Response, false, nil)
	if err != nil {
		return nil, httperrors.NewGeneralError(err)
	}

	return nil, nil
}

func (self *SImage) getMoreDetails(out api.ImageDetails) api.ImageDetails {
	properties, err := ImagePropertyManager.GetProperties(self.Id)
	if err != nil {
		log.Errorf("ImagePropertyManager.GetProperties fail %s", err)
	}
	out.Properties = properties

	if self.PendingDeleted {
		pendingDeletedAt := self.PendingDeletedAt.Add(time.Second * time.Duration(options.Options.PendingDeleteExpireSeconds))
		out.AutoDeleteAt = pendingDeletedAt
	}

	var ossChksum = self.OssChecksum
	if len(self.OssChecksum) == 0 {
		ossChksum = self.Checksum
	}
	out.OssChecksum = ossChksum
	out.DisableDelete = self.Protected.Bool()
	return out
}

func (manager *SImageManager) FetchCustomizeColumns(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	query jsonutils.JSONObject,
	objs []interface{},
	fields stringutils2.SSortedStrings,
	isList bool,
) []api.ImageDetails {
	rows := make([]api.ImageDetails, len(objs))

	virtRows := manager.SSharableVirtualResourceBaseManager.FetchCustomizeColumns(ctx, userCred, query, objs, fields, isList)

	for i := range rows {
		image := objs[i].(*SImage)
		rows[i] = api.ImageDetails{
			SharableVirtualResourceDetails: virtRows[i],
		}
		rows[i] = image.getMoreDetails(rows[i])
	}

	return rows
}

func (self *SImage) GetExtraDetails(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	query jsonutils.JSONObject,
	isList bool,
) (api.ImageDetails, error) {
	return api.ImageDetails{}, nil
}

func (self *SImage) GetExtraDetailsHeaders(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) map[string]string {
	headers := make(map[string]string)

	details := ImageManager.FetchCustomizeColumns(ctx, userCred, query, []interface{}{self}, nil, false)
	extra := jsonutils.Marshal(details[0]).(*jsonutils.JSONDict)
	for _, k := range extra.SortedKeys() {
		if k == "properties" {
			continue
		}
		val, _ := extra.GetString(k)
		if len(val) > 0 {
			headers[fmt.Sprintf("%s%s", modules.IMAGE_META, k)] = val
		}
	}

	jsonDict := jsonutils.Marshal(self).(*jsonutils.JSONDict)
	fields, _ := db.GetDetailFields(self.GetModelManager(), userCred)
	for _, k := range jsonDict.SortedKeys() {
		if utils.IsInStringArray(k, fields) {
			val, _ := jsonDict.GetString(k)
			if len(val) > 0 {
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, k)] = val
			}
		}
	}

	formatStr := jsonutils.GetAnyString(query, []string{"format", "disk_format"})
	if len(formatStr) > 0 {
		subimg := ImageSubformatManager.FetchSubImage(self.Id, formatStr)
		if subimg != nil {
			headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "disk_format")] = formatStr
			isTorrent := jsonutils.QueryBoolean(query, "torrent", false)
			if !isTorrent {
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "status")] = subimg.Status
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "size")] = fmt.Sprintf("%d", subimg.Size)
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "checksum")] = subimg.Checksum
			} else {
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "status")] = subimg.TorrentStatus
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "size")] = fmt.Sprintf("%d", subimg.TorrentSize)
				headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "checksum")] = subimg.TorrentChecksum
			}
		}
	}

	// none of subimage business
	var ossChksum = self.OssChecksum
	if len(self.OssChecksum) == 0 {
		ossChksum = self.Checksum
	}
	headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "oss_checksum")] = ossChksum

	properties, _ := ImagePropertyManager.GetProperties(self.Id)
	if len(properties) > 0 {
		for k, v := range properties {
			headers[fmt.Sprintf("%s%s", modules.IMAGE_META_PROPERTY, k)] = v
		}
	}

	if self.PendingDeleted {
		pendingDeletedAt := self.PendingDeletedAt.Add(time.Second * time.Duration(options.Options.PendingDeleteExpireSeconds))
		headers[fmt.Sprintf("%s%s", modules.IMAGE_META, "auto_delete_at")] = timeutils.FullIsoTime(pendingDeletedAt)
	}

	return headers
}

func (manager *SImageManager) ValidateCreateData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, query jsonutils.JSONObject, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	input := apis.VirtualResourceCreateInput{}
	err := data.Unmarshal(&input)
	if err != nil {
		return nil, httperrors.NewInternalServerError("unmarshal StandaloneResourceCreateInput fail %s", err)
	}
	input, err = manager.SVirtualResourceBaseManager.ValidateCreateData(ctx, userCred, ownerId, query, input)
	if err != nil {
		return nil, err
	}
	data.Update(jsonutils.Marshal(input))

	// If this image is the part of guest image (contains "guest_image_id"),
	// we do not need to check and set pending quota
	// because that pending quota has been checked and set in SGuestImage.ValidateCreateData
	if !data.Contains("guest_image_id") {
		pendingUsage := SQuota{Image: 1}
		keys := imageCreateInput2QuotaKeys(data, ownerId)
		pendingUsage.SetKeys(keys)
		if err := quotas.CheckSetPendingQuota(ctx, userCred, &pendingUsage); err != nil {
			return nil, httperrors.NewOutOfQuotaError("%s", err)
		}
	}

	return data, nil
}

func (self *SImage) CustomizeCreate(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, query jsonutils.JSONObject, data jsonutils.JSONObject) error {
	err := self.SVirtualResourceBase.CustomizeCreate(ctx, userCred, ownerId, query, data)
	if err != nil {
		return err
	}
	self.Status = api.IMAGE_STATUS_QUEUED
	self.Owner = self.ProjectId
	// if belong to a guest image,
	if data.Contains("guest_image_id") {
		self.IsGuestImage = tristate.True
	}
	return nil
}

func (self *SImage) GetPath(format string) string {
	path := filepath.Join(options.Options.FilesystemStoreDatadir, self.Id)
	if len(format) > 0 {
		path = fmt.Sprintf("%s.%s", path, format)
	}
	return path
}

func (self *SImage) OnJointFailed(ctx context.Context, userCred mcclient.TokenCredential) {
	log.Errorf("create joint of image and guest image failed")
	self.SetStatus(userCred, api.IMAGE_STATUS_KILLED, "")
}

func (self *SImage) OnSaveFailed(ctx context.Context, userCred mcclient.TokenCredential, msg string) {
	self.saveFailed(userCred, msg)
	logclient.AddActionLogWithContext(ctx, self, logclient.ACT_IMAGE_SAVE, msg, userCred, false)
}

func (self *SImage) OnSaveTaskFailed(task taskman.ITask, userCred mcclient.TokenCredential, msg string) {
	self.saveFailed(userCred, msg)
	logclient.AddActionLogWithStartable(task, self, logclient.ACT_IMAGE_SAVE, msg, userCred, false)
}

func (self *SImage) OnSaveSuccess(ctx context.Context, userCred mcclient.TokenCredential, msg string) {
	self.saveSuccess(userCred, msg)
	logclient.AddActionLogWithContext(ctx, self, logclient.ACT_IMAGE_SAVE, msg, userCred, true)
}

func (self *SImage) OnSaveTaskSuccess(task taskman.ITask, userCred mcclient.TokenCredential, msg string) {
	self.saveSuccess(userCred, msg)
	logclient.AddActionLogWithStartable(task, self, logclient.ACT_IMAGE_SAVE, msg, userCred, true)
}

func (self *SImage) saveSuccess(userCred mcclient.TokenCredential, msg string) {
	// do not set this status, until image converting complete
	// self.SetStatus(userCred, api.IMAGE_STATUS_ACTIVE, msg)
	db.OpsLog.LogEvent(self, db.ACT_SAVE, msg, userCred)
}

func (self *SImage) saveFailed(userCred mcclient.TokenCredential, msg string) {
	log.Errorf(msg)
	self.SetStatus(userCred, api.IMAGE_STATUS_KILLED, msg)
	db.OpsLog.LogEvent(self, db.ACT_SAVE_FAIL, msg, userCred)
}

func (self *SImage) saveImageFromStream(localPath string, reader io.Reader, calChecksum bool) (*streamutils.SStreamProperty, error) {
	fp, err := os.Create(localPath)
	if err != nil {
		return nil, err
	}
	defer fp.Close()
	lastSaveTime := time.Now()
	return streamutils.StreamPipe(reader, fp, calChecksum, func(saved int64) {
		now := time.Now()
		if now.Sub(lastSaveTime) > 5*time.Second {
			self.saveSize(saved)
			lastSaveTime = now
		}
	})
}

func (self *SImage) saveSize(newSize int64) error {
	_, err := db.Update(self, func() error {
		self.Size = newSize
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "Update size")
	}
	return nil
}

//Image always do probe and customize after save from stream
func (self *SImage) SaveImageFromStream(reader io.Reader, calChecksum bool) error {
	localPath := self.GetPath("")

	sp, err := self.saveImageFromStream(localPath, reader, calChecksum)
	if err != nil {
		log.Errorf("saveImageFromStream fail %s", err)
		return err
	}

	virtualSizeBytes := int64(0)
	format := ""
	img, err := qemuimg.NewQemuImage(localPath)
	if err != nil {
		return err
	}
	format = string(img.Format)
	virtualSizeBytes = img.SizeBytes

	var fastChksum string
	if calChecksum {
		fastChksum, err = fileutils2.FastCheckSum(localPath)
		if err != nil {
			return err
		}
	}

	db.Update(self, func() error {
		self.Size = sp.Size
		if calChecksum {
			self.Checksum = sp.CheckSum
			self.FastHash = fastChksum
		}
		self.Location = fmt.Sprintf("%s%s", LocalFilePrefix, localPath)
		if len(format) > 0 {
			self.DiskFormat = format
		}
		if virtualSizeBytes > 0 {
			self.MinDiskMB = int32(math.Ceil(float64(virtualSizeBytes) / 1024 / 1024))
		}
		return nil
	})

	return nil
}

func (self *SImage) PostCreate(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, query jsonutils.JSONObject, data jsonutils.JSONObject) {
	self.SVirtualResourceBase.PostCreate(ctx, userCred, ownerId, query, data)

	// if SImage belong to a guest image, pending quota will not be set.
	if self.IsGuestImage.IsFalse() {
		pendingUsage := SQuota{Image: 1}
		keys := imageCreateInput2QuotaKeys(data, ownerId)
		pendingUsage.SetKeys(keys)
		cancelUsage := SQuota{Image: 1}
		keys = self.GetQuotaKeys()
		cancelUsage.SetKeys(keys)
		quotas.CancelPendingUsage(ctx, userCred, &pendingUsage, &cancelUsage, true)
	}

	if data.Contains("properties") {
		// update properties
		props, _ := data.Get("properties")
		err := ImagePropertyManager.SaveProperties(ctx, userCred, self.Id, props)
		if err != nil {
			log.Warningf("save properties error %s", err)
		}
	}

	appParams := appsrv.AppContextGetParams(ctx)
	if appParams.Request.ContentLength > 0 {
		db.OpsLog.LogEvent(self, db.ACT_SAVING, "create upload", userCred)
		self.SetStatus(userCred, api.IMAGE_STATUS_SAVING, "create upload")

		err := self.SaveImageFromStream(appParams.Request.Body, false)
		if err != nil {
			self.OnSaveFailed(ctx, userCred, fmt.Sprintf("create upload fail %s", err))
			return
		}

		self.OnSaveSuccess(ctx, userCred, "create upload success")
		self.ImageProbeAndCustomization(ctx, userCred, true)
	} else {
		copyFrom := appParams.Request.Header.Get(modules.IMAGE_META_COPY_FROM)
		if len(copyFrom) > 0 {
			self.startImageCopyFromUrlTask(ctx, userCred, copyFrom, "")
		}
	}

	// This image is belong to some guestImage.
	// Code below must be run without upload.
	if data.Contains("guest_image_id") {
		guestImageId, _ := data.GetString("guest_image_id")
		_, err := GuestImageJointManager.CreateGuestImageJoint(ctx, guestImageId, self.Id)
		if err != nil {
			self.OnJointFailed(ctx, userCred)
		}
	}
}

// After image probe and customization, image size and checksum changed
// will recalculate checksum in the end
func (self *SImage) ImageProbeAndCustomization(
	ctx context.Context, userCred mcclient.TokenCredential, doConvertAfterProbe bool,
) error {
	data := jsonutils.NewDict()
	data.Set("do_convert", jsonutils.NewBool(doConvertAfterProbe))
	task, err := taskman.TaskManager.NewTask(
		ctx, "ImageProbeTask", self, userCred, data, "", "", nil)
	if err != nil {
		return err
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SImage) ValidateUpdateData(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	if self.Status != api.IMAGE_STATUS_QUEUED {
		if !self.CanUpdate(data) {
			return nil, httperrors.NewForbiddenError("image is the part of guest imgae")
		}
		appParams := appsrv.AppContextGetParams(ctx)
		if appParams != nil && appParams.Request.ContentLength > 0 {
			return nil, httperrors.NewInvalidStatusError("cannot upload in status %s", self.Status)
		}
	} else {
		appParams := appsrv.AppContextGetParams(ctx)
		if appParams != nil {
			isProbe := true
			if self.IsData.IsTrue() {
				isProbe = false
			}
			if appParams.Request.ContentLength > 0 {
				self.SetStatus(userCred, api.IMAGE_STATUS_SAVING, "update start upload")
				// If isProbe is true calculating checksum is not necessary wheng saving from stream,
				// otherwise, it is needed.
				err := self.SaveImageFromStream(appParams.Request.Body, !isProbe)
				if err != nil {
					self.OnSaveFailed(ctx, userCred, fmt.Sprintf("update upload failed %s", err))
					return nil, httperrors.NewGeneralError(err)
				}
				self.OnSaveSuccess(ctx, userCred, "update upload success")
				if !isProbe {
					// no probe
					self.SetStatus(userCred, api.IMAGE_STATUS_ACTIVE, "data disk image upload success")
				} else {
					data.Remove("status")
					// For guest image, DoConvertAfterProbe is not necessary.
					if self.IsGuestImage.IsTrue() {
						self.ImageProbeAndCustomization(ctx, userCred, false)
					} else {
						self.ImageProbeAndCustomization(ctx, userCred, true)
					}
				}
			} else {
				copyFrom := appParams.Request.Header.Get(modules.IMAGE_META_COPY_FROM)
				if len(copyFrom) > 0 {
					err := self.startImageCopyFromUrlTask(ctx, userCred, copyFrom, "")
					if err != nil {
						self.OnSaveFailed(ctx, userCred, fmt.Sprintf("update copy from url failed %s", err))
						return nil, httperrors.NewGeneralError(err)
					}
				}
			}
		}
	}
	input := apis.VirtualResourceBaseUpdateInput{}
	err := data.Unmarshal(&input)
	if err != nil {
		return nil, errors.Wrap(err, "Unmarshal")
	}
	input, err = self.SVirtualResourceBase.ValidateUpdateData(ctx, userCred, query, input)
	if err != nil {
		return nil, errors.Wrap(err, "SVirtualResourceBase.ValidateUpdateData")
	}
	data.Update(jsonutils.Marshal(input))

	return data, nil
}

func (self *SImage) PreUpdate(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) {
	self.SVirtualResourceBase.PreUpdate(ctx, userCred, query, data)
}

func (self *SImage) PostUpdate(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) {
	self.SVirtualResourceBase.PostUpdate(ctx, userCred, query, data)

	if data.Contains("properties") {
		// update properties
		props, _ := data.Get("properties")
		err := ImagePropertyManager.SaveProperties(ctx, userCred, self.Id, props)
		if err != nil {
			log.Errorf("save properties error %s", err)
		}
	}
}

func (self *SImage) AllowDeleteItem(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) bool {
	overridePendingDelete := false
	purge := false
	if query != nil {
		overridePendingDelete = jsonutils.QueryBoolean(query, "override_pending_delete", false)
		purge = jsonutils.QueryBoolean(query, "purge", false)
	}
	if (overridePendingDelete || purge) && !db.IsAdminAllowDelete(userCred, self) {
		return false
	}
	return self.IsOwner(userCred) || db.IsAdminAllowDelete(userCred, self)
}

func (self *SImage) ValidateDeleteCondition(ctx context.Context) error {
	if self.Protected.IsTrue() {
		return httperrors.NewForbiddenError("image is protected")
	}
	if self.IsGuestImage.IsTrue() {
		return httperrors.NewForbiddenError("image is the part of guest image")
	}
	if self.IsShared() {
		return httperrors.NewForbiddenError("image is shared")
	}
	return self.SVirtualResourceBase.ValidateDeleteCondition(ctx)
}

func (self *SImage) Delete(ctx context.Context, userCred mcclient.TokenCredential) error {
	log.Infof("image delete do nothing")
	return nil
}

func (self *SImage) RealDelete(ctx context.Context, userCred mcclient.TokenCredential) error {
	return self.SVirtualResourceBase.Delete(ctx, userCred)
}

func (self *SImage) CustomizeDelete(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) error {
	overridePendingDelete := false
	purge := false
	if query != nil {
		overridePendingDelete = jsonutils.QueryBoolean(query, "override_pending_delete", false)
		purge = jsonutils.QueryBoolean(query, "purge", false)
	}
	if utils.IsInStringArray(self.Status, []string{
		api.IMAGE_STATUS_KILLED,
		api.IMAGE_STATUS_QUEUED,
	}) {
		overridePendingDelete = true
	}
	return self.startDeleteImageTask(ctx, userCred, "", purge, overridePendingDelete)
}

func (self *SImage) startDeleteImageTask(ctx context.Context, userCred mcclient.TokenCredential, parentTaskId string, isPurge bool, overridePendingDelete bool) error {
	params := jsonutils.NewDict()
	if isPurge {
		params.Add(jsonutils.JSONTrue, "purge")
	}
	if overridePendingDelete {
		params.Add(jsonutils.JSONTrue, "override_pending_delete")
	}
	params.Add(jsonutils.NewString(self.Status), "image_status")

	self.SetStatus(userCred, api.IMAGE_STATUS_DEACTIVATED, "")

	task, err := taskman.TaskManager.NewTask(ctx, "ImageDeleteTask", self, userCred, params, parentTaskId, "", nil)
	if err != nil {
		return err
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SImage) startImageCopyFromUrlTask(ctx context.Context, userCred mcclient.TokenCredential, copyFrom string, parentTaskId string) error {
	params := jsonutils.NewDict()
	params.Add(jsonutils.NewString(copyFrom), "copy_from")

	msg := fmt.Sprintf("copy from url %s", copyFrom)
	self.SetStatus(userCred, api.IMAGE_STATUS_SAVING, msg)
	db.OpsLog.LogEvent(self, db.ACT_SAVING, msg, userCred)

	task, err := taskman.TaskManager.NewTask(ctx, "ImageCopyFromUrlTask", self, userCred, params, parentTaskId, "", nil)
	if err != nil {
		return err
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SImage) StartImageCheckTask(ctx context.Context, userCred mcclient.TokenCredential, parentTaskId string) error {
	task, err := taskman.TaskManager.NewTask(ctx, "ImageCheckTask", self, userCred, nil, parentTaskId, "", nil)
	if err != nil {
		return err
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SImage) StartImageConvertTask(ctx context.Context, userCred mcclient.TokenCredential, parentTaskId string) error {
	err := self.MigrateSubImage()
	if err != nil {
		return err
	}
	err = self.MakeSubImages()
	if err != nil {
		return err
	}

	task, err := taskman.TaskManager.NewTask(ctx, "ImageConvertTask", self, userCred, nil, parentTaskId, "", nil)
	if err != nil {
		return err
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SImage) AllowPerformCancelDelete(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) bool {
	return db.IsAdminAllowPerform(userCred, self, "cancel-delete") && self.IsGuestImage.IsFalse()
}

func (self *SImage) PerformCancelDelete(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	if self.PendingDeleted {
		err := self.DoCancelPendingDelete(ctx, userCred)
		return nil, err
	}
	return nil, nil
}

func (manager *SImageManager) getExpiredPendingDeleteDisks() []SImage {
	deadline := time.Now().Add(time.Duration(options.Options.PendingDeleteExpireSeconds*-1) * time.Second)

	q := manager.Query()
	// those images part of guest image will be clean in GuestImageManager.CleanPendingDeleteImages
	q = q.IsTrue("pending_deleted").LT("pending_deleted_at",
		deadline).Limit(options.Options.PendingDeleteMaxCleanBatchSize).IsFalse("is_guest_image")

	disks := make([]SImage, 0)
	err := db.FetchModelObjects(ImageManager, q, &disks)
	if err != nil {
		log.Errorf("fetch disks error %s", err)
		return nil
	}

	return disks
}

func (manager *SImageManager) CleanPendingDeleteImages(ctx context.Context, userCred mcclient.TokenCredential, isStart bool) {
	disks := manager.getExpiredPendingDeleteDisks()
	if disks == nil {
		return
	}
	for i := 0; i < len(disks); i += 1 {
		// clean pendingdelete so that overridePendingDelete is true
		disks[i].startDeleteImageTask(ctx, userCred, "", false, true)
	}
}

func (self *SImage) DoPendingDelete(ctx context.Context, userCred mcclient.TokenCredential) error {
	err := self.SVirtualResourceBase.DoPendingDelete(ctx, userCred)
	if err != nil {
		return err
	}
	_, err = db.Update(self, func() error {
		self.Status = api.IMAGE_STATUS_PENDING_DELETE
		return nil
	})
	return err
}

func (self *SImage) DoCancelPendingDelete(ctx context.Context, userCred mcclient.TokenCredential) error {
	err := self.SVirtualResourceBase.DoCancelPendingDelete(ctx, userCred)
	if err != nil {
		return err
	}
	_, err = db.Update(self, func() error {
		self.Status = api.IMAGE_STATUS_ACTIVE
		return nil
	})
	return err
}

type SImageUsage struct {
	Count int64
	Size  int64
}

func (manager *SImageManager) count(scope rbacutils.TRbacScope, ownerId mcclient.IIdentityProvider, status string, isISO tristate.TriState, pendingDelete bool) map[string]SImageUsage {
	sq := manager.Query("id")
	switch scope {
	case rbacutils.ScopeSystem:
		// do nothing
	case rbacutils.ScopeDomain:
		sq = sq.Equals("domain_id", ownerId.GetProjectDomainId())
	case rbacutils.ScopeProject:
		sq = sq.Equals("tenant_id", ownerId.GetProjectId())
	}
	if len(status) > 0 {
		sq = sq.Equals("status", status)
	}
	sq = sq.NotEquals("status", api.IMAGE_STATUS_KILLED)
	if pendingDelete {
		sq = sq.IsTrue("pending_deleted")
	} else {
		sq = sq.IsFalse("pending_deleted")
	}
	if isISO.IsTrue() {
		sq = sq.Equals("disk_format", "iso")
	} else if isISO.IsFalse() {
		sq = sq.NotEquals("disk_format", "iso")
	}
	subimages := ImageSubformatManager.Query().SubQuery()
	q := subimages.Query(subimages.Field("format"),
		sqlchemy.COUNT("count"),
		sqlchemy.SUM("size", subimages.Field("size")))
	q = q.In("image_id", sq.SubQuery())
	q = q.GroupBy(subimages.Field("format"))
	type sFormatImageUsage struct {
		Format string
		Count  int64
		Size   int64
	}
	var usages []sFormatImageUsage
	err := q.All(&usages)
	if err != nil {
		log.Errorf("query usage fail %s", err)
		return nil
	}
	ret := make(map[string]SImageUsage)
	totalSize := int64(0)
	for _, u := range usages {
		ret[u.Format] = SImageUsage{Count: u.Count, Size: u.Size}
		totalSize += u.Size
	}
	cnt, _ := sq.CountWithError()
	ret["total"] = SImageUsage{Count: int64(cnt), Size: totalSize}
	return ret
}

func expandUsageCount(usages map[string]int64, prefix, imgType, state string, count map[string]SImageUsage) {
	for k, u := range count {
		key := []string{}
		if len(prefix) > 0 {
			key = append(key, prefix)
		}
		key = append(key, imgType)
		if len(state) > 0 {
			key = append(key, state)
		}
		key = append(key, k)
		countKey := strings.Join(append(key, "count"), ".")
		sizeKey := strings.Join(append(key, "size"), ".")
		usages[countKey] = u.Count
		usages[sizeKey] = u.Size
	}
}

func (manager *SImageManager) Usage(scope rbacutils.TRbacScope, ownerId mcclient.IIdentityProvider, prefix string) map[string]int64 {
	usages := make(map[string]int64)
	count := manager.count(scope, ownerId, api.IMAGE_STATUS_ACTIVE, tristate.False, false)
	expandUsageCount(usages, prefix, "img", "", count)
	count = manager.count(scope, ownerId, api.IMAGE_STATUS_ACTIVE, tristate.True, false)
	expandUsageCount(usages, prefix, "iso", "", count)
	count = manager.count(scope, ownerId, api.IMAGE_STATUS_ACTIVE, tristate.None, false)
	expandUsageCount(usages, prefix, "imgiso", "", count)
	count = manager.count(scope, ownerId, "", tristate.False, true)
	expandUsageCount(usages, prefix, "img", "pending_delete", count)
	count = manager.count(scope, ownerId, "", tristate.True, true)
	expandUsageCount(usages, prefix, "iso", "pending_delete", count)
	count = manager.count(scope, ownerId, "", tristate.None, true)
	expandUsageCount(usages, prefix, "imgiso", "pending_delete", count)
	return usages
}

func (self *SImage) GetImageType() api.TImageType {
	if self.DiskFormat == string(qemuimg.ISO) {
		return api.ImageTypeISO
	} else {
		return api.ImageTypeTemplate
	}
}

func (self *SImage) newSubformat(format qemuimg.TImageFormat, migrate bool) error {
	subformat := &SImageSubformat{}
	subformat.SetModelManager(ImageSubformatManager, subformat)

	subformat.ImageId = self.Id
	subformat.Format = string(format)

	if migrate {
		subformat.Size = self.Size
		subformat.Checksum = self.Checksum
		subformat.FastHash = self.FastHash
		subformat.Status = api.IMAGE_STATUS_ACTIVE
		subformat.Location = self.Location
	} else {
		subformat.Status = api.IMAGE_STATUS_QUEUED
	}

	subformat.TorrentStatus = api.IMAGE_STATUS_QUEUED

	err := ImageSubformatManager.TableSpec().Insert(subformat)
	if err != nil {
		log.Errorf("fail to make subformat %s: %s", format, err)
		return err
	}
	return nil
}

func (self *SImage) MigrateSubImage() error {
	if !qemuimg.IsSupportedImageFormat(self.DiskFormat) {
		log.Warningf("Unsupported image format %s, no need to migrate", self.DiskFormat)
		return nil
	}

	subimg := ImageSubformatManager.FetchSubImage(self.Id, self.DiskFormat)
	if subimg != nil {
		return nil
	}

	imgInst, err := self.getQemuImage()
	if err != nil {
		return err
	}
	if self.GetImageType() != api.ImageTypeISO && imgInst.IsSparse() {
		// need to convert again
		return self.newSubformat(qemuimg.String2ImageFormat(self.DiskFormat), false)
	} else {
		localPath := self.getLocalLocation()
		if !strings.HasSuffix(localPath, fmt.Sprintf(".%s", self.DiskFormat)) {
			newLocalpath := fmt.Sprintf("%s.%s", localPath, self.DiskFormat)
			cmd := exec.Command("mv", "-f", localPath, newLocalpath)
			err := cmd.Run()
			if err != nil {
				return err
			}
			_, err = db.Update(self, func() error {
				self.Location = fmt.Sprintf("%s%s", LocalFilePrefix, newLocalpath)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return self.newSubformat(qemuimg.String2ImageFormat(self.DiskFormat), true)
	}
}

func (self *SImage) MakeSubImages() error {
	if self.GetImageType() == api.ImageTypeISO {
		return nil
	}
	log.Debugf("[MakeSubImages] convert image to %#v", options.Options.TargetImageFormats)
	for _, format := range options.Options.TargetImageFormats {
		if !qemuimg.IsSupportedImageFormat(format) {
			continue
		}
		if format != self.DiskFormat {
			// need to create a record
			subformat := ImageSubformatManager.FetchSubImage(self.Id, format)
			if subformat == nil {
				err := self.newSubformat(qemuimg.String2ImageFormat(format), false)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (self *SImage) ConvertAllSubformats() error {
	subimgs := ImageSubformatManager.GetAllSubImages(self.Id)
	for i := 0; i < len(subimgs); i += 1 {
		if !utils.IsInStringArray(subimgs[i].Format, options.Options.TargetImageFormats) {
			continue
		}
		err := subimgs[i].DoConvert(self)
		if err != nil {
			return err
		}
	}
	return nil
}

func (self *SImage) getLocalLocation() string {
	if len(self.Location) > len(LocalFilePrefix) {
		return self.Location[len(LocalFilePrefix):]
	}
	return ""
}

func (self *SImage) getQemuImage() (*qemuimg.SQemuImage, error) {
	return qemuimg.NewQemuImageWithIOLevel(self.getLocalLocation(), qemuimg.IONiceIdle)
}

func (self *SImage) StopTorrents() {
	subimgs := ImageSubformatManager.GetAllSubImages(self.Id)
	for i := 0; i < len(subimgs); i += 1 {
		subimgs[i].StopTorrent()
	}
}

func (self *SImage) seedTorrents() {
	subimgs := ImageSubformatManager.GetAllSubImages(self.Id)
	for i := 0; i < len(subimgs); i += 1 {
		subimgs[i].seedTorrent(self.Id)
	}
}

func (self *SImage) RemoveFiles() error {
	subimgs := ImageSubformatManager.GetAllSubImages(self.Id)
	for i := 0; i < len(subimgs); i += 1 {
		subimgs[i].StopTorrent()
		err := subimgs[i].RemoveFiles()
		if err != nil {
			return err
		}
	}
	filePath := self.getLocalLocation()
	if len(filePath) == 0 {
		filePath = self.GetPath("")
	}
	if len(filePath) > 0 && fileutils2.IsFile(filePath) {
		return os.Remove(filePath)
	}
	return nil
}

func (manager *SImageManager) getAllAliveImages() []SImage {
	images := make([]SImage, 0)
	q := manager.Query().NotIn("status", api.ImageDeadStatus)
	err := db.FetchModelObjects(manager, q, &images)
	if err != nil {
		log.Errorf("fail to query active images %s", err)
		return nil
	}
	return images
}

func CheckImages() {
	images := ImageManager.getAllAliveImages()
	for i := 0; i < len(images); i += 1 {
		log.Debugf("convert image subformats %s", images[i].Name)
		images[i].StartImageCheckTask(context.TODO(), auth.AdminCredential(), "")
	}
}

func (self *SImage) AllowGetDetailsSubformats(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) bool {
	return self.IsOwner(userCred) || db.IsAdminAllowGetSpec(userCred, self, "subformats")
}

func (self *SImage) GetDetailsSubformats(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	subimgs := ImageSubformatManager.GetAllSubImages(self.Id)
	ret := make([]SImageSubformatDetails, len(subimgs))
	for i := 0; i < len(subimgs); i += 1 {
		ret[i] = subimgs[i].GetDetails()
	}
	return jsonutils.Marshal(ret), nil
}

// 磁盘镜像列表
func (manager *SImageManager) ListItemFilter(
	ctx context.Context,
	q *sqlchemy.SQuery,
	userCred mcclient.TokenCredential,
	query api.ImageListInput,
) (*sqlchemy.SQuery, error) {
	q, err := manager.SSharableVirtualResourceBaseManager.ListItemFilter(ctx, q, userCred, query.SharableVirtualResourceListInput)
	if err != nil {
		return nil, errors.Wrap(err, "SSharableVirtualResourceBaseManager.ListItemFilter")
	}
	if len(query.DiskFormats) > 0 {
		q = q.In("disk_format", query.DiskFormats)
	}
	if query.Uefi != nil && *query.Uefi {
		imagePropertyQ := ImagePropertyManager.Query().
			Equals("name", api.IMAGE_UEFI_SUPPORT).Equals("value", "true").SubQuery()
		q = q.Join(imagePropertyQ, sqlchemy.Equals(q.Field("id"), imagePropertyQ.Field("image_id")))
	}
	if query.IsStandard != nil {
		if *query.IsStandard {
			q = q.IsTrue("is_standard")
		} else {
			q = q.IsFalse("is_standard")
		}
	}
	if query.Protected != nil {
		if *query.Protected {
			q = q.IsTrue("protected")
		} else {
			q = q.IsFalse("protected")
		}
	}
	if query.IsGuestImage != nil {
		if *query.IsGuestImage {
			q = q.IsTrue("is_guest_image")
		} else {
			q = q.IsFalse("is_guest_image")
		}
	}
	if query.IsData != nil {
		if *query.IsData {
			q = q.IsTrue("is_data")
		} else {
			q = q.IsFalse("is_data")
		}
	}
	return q, nil
}

func (manager *SImageManager) OrderByExtraFields(
	ctx context.Context,
	q *sqlchemy.SQuery,
	userCred mcclient.TokenCredential,
	query api.ImageListInput,
) (*sqlchemy.SQuery, error) {
	var err error

	q, err = manager.SSharableVirtualResourceBaseManager.OrderByExtraFields(ctx, q, userCred, query.SharableVirtualResourceListInput)
	if err != nil {
		return nil, errors.Wrap(err, "SSharableVirtualResourceBaseManager.OrderByExtraFields")
	}

	return q, nil
}

func (manager *SImageManager) QueryDistinctExtraField(q *sqlchemy.SQuery, field string) (*sqlchemy.SQuery, error) {
	var err error

	q, err = manager.SSharableVirtualResourceBaseManager.QueryDistinctExtraField(q, field)
	if err == nil {
		return q, nil
	}

	return q, httperrors.ErrNotFound
}

func isActive(localPath string, size int64, chksum string, fastHash string, useFastHash bool) bool {
	if len(localPath) == 0 || !fileutils2.Exists(localPath) {
		log.Errorf("invalid file: %s", localPath)
		return false
	}
	if size != fileutils2.FileSize(localPath) {
		log.Errorf("size mistmatch: %s", localPath)
		return false
	}
	if useFastHash && len(fastHash) > 0 {
		fhash, err := fileutils2.FastCheckSum(localPath)
		if err != nil {
			log.Errorf("IsActive fastChecksum fail %s for %s", err, localPath)
			return false
		}
		if fastHash != fhash {
			log.Errorf("IsActive fastChecksum mismatch for %s", localPath)
			return false
		}
	} else {
		md5sum, err := fileutils2.MD5(localPath)
		if err != nil {
			log.Errorf("IsActive md5 fail %s for %s", err, localPath)
			return false
		}
		if chksum != md5sum {
			log.Errorf("IsActive checksum mismatch: %s", localPath)
			return false
		}
	}
	return true
}

func (self *SImage) IsIso() (error, bool) {
	if self.DiskFormat == string(api.ImageTypeISO) {
		return nil, true
	}
	img, err := qemuimg.NewQemuImage(self.GetPath(""))
	if err != nil {
		return errors.Wrap(err, "open image failed"), false
	}
	return nil, img.Format == qemuimg.ISO
}

func (self *SImage) isActive(useFast bool) bool {
	return isActive(self.getLocalLocation(), self.Size, self.Checksum, self.FastHash, useFast)
}

func (self *SImage) DoCheckStatus(ctx context.Context, userCred mcclient.TokenCredential, useFast bool) {
	if utils.IsInStringArray(self.Status, api.ImageDeadStatus) {
		return
	}
	if self.isActive(useFast) {
		if self.Status != api.IMAGE_STATUS_ACTIVE {
			self.SetStatus(userCred, api.IMAGE_STATUS_ACTIVE, "check active")
		}
		if len(self.FastHash) == 0 {
			fastHash, err := fileutils2.FastCheckSum(self.getLocalLocation())
			if err != nil {
				log.Errorf("DoCheckStatus fileutils2.FastChecksum fail %s", err)
			} else {
				_, err := db.Update(self, func() error {
					self.FastHash = fastHash
					return nil
				})
				if err != nil {
					log.Errorf("DoCheckStatus save FastHash fail %s", err)
				}
			}
		}
		img, err := qemuimg.NewQemuImage(self.getLocalLocation())
		if err == nil {
			format := string(img.Format)
			virtualSizeMB := int32(img.SizeBytes / 1024 / 1024)
			if (len(format) > 0 && self.DiskFormat != format) || (virtualSizeMB > 0 && self.MinDiskMB != virtualSizeMB) {
				db.Update(self, func() error {
					if len(format) > 0 {
						self.DiskFormat = format
					}
					if virtualSizeMB > 0 && self.MinDiskMB < virtualSizeMB {
						self.MinDiskMB = virtualSizeMB
					}
					return nil
				})
			}
		} else {
			log.Warningf("fail to check image size of %s(%s)", self.Id, self.Name)
		}
	} else {
		if self.Status != api.IMAGE_STATUS_QUEUED {
			self.SetStatus(userCred, api.IMAGE_STATUS_QUEUED, "check inactive")
		}
	}
	needConvert := false
	subimgs := ImageSubformatManager.GetAllSubImages(self.Id)
	// for image the part of a guest image, convert is not necessary.
	if len(subimgs) == 0 && self.IsGuestImage.IsFalse() {
		needConvert = true
	}
	for i := 0; i < len(subimgs); i += 1 {
		subimgs[i].checkStatus(useFast)
		if (subimgs[i].Status != api.IMAGE_STATUS_ACTIVE || subimgs[i].TorrentStatus != api.IMAGE_STATUS_ACTIVE) && utils.IsInStringArray(subimgs[i].Format, options.Options.TargetImageFormats) {
			needConvert = true
		}
	}
	if self.Status == api.IMAGE_STATUS_ACTIVE {
		if needConvert {
			log.Infof("Image %s is active and need convert", self.Name)
			self.StartImageConvertTask(ctx, userCred, "")
		} else if options.Options.EnableTorrentService {
			self.seedTorrents()
		}
	}
}

func (self *SImage) AllowPerformMarkStandard(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	query jsonutils.JSONObject,
	data jsonutils.JSONObject,
) bool {
	return db.IsAdminAllowPerform(userCred, self, "mark-standard") && self.IsGuestImage.IsFalse()
}

func (self *SImage) PerformMarkStandard(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	query jsonutils.JSONObject,
	data jsonutils.JSONObject,
) (jsonutils.JSONObject, error) {
	isStandard := jsonutils.QueryBoolean(data, "is_standard", false)
	if !self.IsStandard.IsTrue() && isStandard {
		input := apis.PerformPublicInput{
			Scope: "system",
		}
		_, err := self.PerformPublic(ctx, userCred, query, input)
		if err != nil {
			return nil, errors.Wrap(err, "PerformPublic")
		}
		diff, err := db.Update(self, func() error {
			self.IsStandard = tristate.True
			return nil
		})
		if err != nil {
			return nil, httperrors.NewGeneralError(err)
		}
		db.OpsLog.LogEvent(self, db.ACT_UPDATE, diff, userCred)
	} else if self.IsStandard.IsTrue() && !isStandard {
		diff, err := db.Update(self, func() error {
			self.IsStandard = tristate.False
			return nil
		})
		if err != nil {
			return nil, httperrors.NewGeneralError(err)
		}
		db.OpsLog.LogEvent(self, db.ACT_UPDATE, diff, userCred)
	}
	return nil, nil
}

func (self *SImage) AllowPerformUpdateTorrentStatus(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) bool {
	return self.IsGuestImage.IsFalse()
}

func (self *SImage) PerformUpdateTorrentStatus(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	formatStr, _ := query.GetString("format")
	if len(formatStr) == 0 {
		return nil, httperrors.NewMissingParameterError("format")
	}
	subimg := ImageSubformatManager.FetchSubImage(self.Id, formatStr)
	if subimg == nil {
		return nil, httperrors.NewResourceNotFoundError("format %s not found", formatStr)
	}
	subimg.SetStatusSeeding(true)
	return nil, nil
}

func (self *SImage) CanUpdate(data jsonutils.JSONObject) bool {
	dict := data.(*jsonutils.JSONDict)
	// Only allow update description for now when Image is part of guest image
	return self.IsGuestImage.IsFalse() || (dict.Length() == 1 && dict.Contains("description"))
}

func (img *SImage) GetQuotaKeys() quotas.IQuotaKeys {
	keys := SImageQuotaKeys{}
	keys.SBaseProjectQuotaKeys = quotas.OwnerIdProjectQuotaKeys(rbacutils.ScopeProject, img.GetOwnerId())
	if img.GetImageType() == api.ImageTypeISO {
		keys.Type = string(api.ImageTypeISO)
	} else {
		keys.Type = string(api.ImageTypeTemplate)
	}
	return keys
}

func imageCreateInput2QuotaKeys(data jsonutils.JSONObject, ownerId mcclient.IIdentityProvider) quotas.IQuotaKeys {
	keys := SImageQuotaKeys{}
	keys.SBaseProjectQuotaKeys = quotas.OwnerIdProjectQuotaKeys(rbacutils.ScopeProject, ownerId)
	format, _ := data.GetString("disk_format")
	if format == string(api.ImageTypeISO) {
		keys.Type = string(api.ImageTypeISO)
	} else if len(format) > 0 {
		keys.Type = string(api.ImageTypeTemplate)
	}
	return keys
}

func (img *SImage) GetUsages() []db.IUsage {
	if img.PendingDeleted || img.Deleted {
		return nil
	}
	usage := SQuota{Image: 1}
	keys := img.GetQuotaKeys()
	usage.SetKeys(keys)
	return []db.IUsage{
		&usage,
	}
}
