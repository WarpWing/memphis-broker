// Credit for The NATS.IO Authors
// Copyright 2021-2022 The Memphis Authors
// Licensed under the Apache License, Version 2.0 (the “License”);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an “AS IS” BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"memphis-broker/analytics"
	"memphis-broker/models"
	"memphis-broker/utils"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type StationsHandler struct{ S *Server }

const (
	stationObjectName = "Station"
)

type StationName struct {
	internal string
	external string
}

func (sn StationName) Ext() string {
	return sn.external
}

func (sn StationName) Intern() string {
	return sn.internal
}

func StationNameFromStr(name string) (StationName, error) {
	extern := strings.ToLower(name)

	err := validateName(extern, stationObjectName)
	if err != nil {
		return StationName{}, err
	}

	intern := replaceDelimiters(name)
	intern = strings.ToLower(intern)

	return StationName{internal: intern, external: extern}, nil
}

func StationNameFromStreamName(streamName string) StationName {
	intern := streamName
	extern := revertDelimiters(intern)

	return StationName{internal: intern, external: extern}
}

func validateRetentionType(retentionType string) error {
	if retentionType != "message_age_sec" && retentionType != "messages" && retentionType != "bytes" {
		return errors.New("retention type can be one of the following message_age_sec/messages/bytes")
	}

	return nil
}

func validateStorageType(storageType string) error {
	if storageType != "file" && storageType != "memory" {
		return errors.New("storage type can be one of the following file/memory")
	}

	return nil
}

func validateReplicas(replicas int) error {
	if replicas > 5 {
		return errors.New("max replicas in a cluster is 5")
	}

	return nil
}

// TODO remove the station resources - functions, connectors
func removeStationResources(s *Server, station models.Station, nonNativeRemoveStreamFunc func() error) error {
	stationName, err := StationNameFromStr(station.Name)
	if err != nil {
		return err
	}

	removeFunc := nonNativeRemoveStreamFunc
	if removeFunc == nil {
		removeFunc = func() error {
			return s.RemoveStream(stationName.Intern())
		}
	}

	err = removeFunc()
	if err != nil {
		return err
	}

	err = s.RemoveStream(fmt.Sprintf(dlsStreamName, stationName.Intern()))
	if err != nil {
		return err
	}

	DeleteTagsFromStation(station.ID)

	_, err = producersCollection.UpdateMany(context.TODO(),
		bson.M{"station_id": station.ID},
		bson.M{"$set": bson.M{"is_active": false, "is_deleted": true}},
	)
	if err != nil {
		return err
	}

	_, err = consumersCollection.UpdateMany(context.TODO(),
		bson.M{"station_id": station.ID},
		bson.M{"$set": bson.M{"is_active": false, "is_deleted": true}},
	)
	if err != nil {
		return err
	}

	err = RemoveAllAuditLogsByStation(station.Name)
	if err != nil {
		serv.Errorf("removeStationResources: Station " + station.Name + ": " + err.Error())
	}

	return nil
}

func (s *Server) createStationDirect(c *client, reply string, msg []byte) {
	var csr createStationRequest
	if err := json.Unmarshal(msg, &csr); err != nil {
		s.Errorf("createStationDirect: failed creating station: %v", err.Error())
		respondWithErr(s, reply, err)
		return
	}
	s.createStationDirectIntern(c, reply, &csr, nil)
}

func (s *Server) createStationDirectIntern(c *client,
	reply string,
	csr *createStationRequest,
	nonNativeCreateStreamFunc func() error) {
	isNative := nonNativeCreateStreamFunc == nil
	jsApiResp := JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}

	stationName, err := StationNameFromStr(csr.StationName)
	if err != nil {
		serv.Warnf("createStationDirect: Station " + csr.StationName + ": " + err.Error())
		jsApiResp.Error = NewJSStreamCreateError(err)
		respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
		return
	}

	exist, _, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("createStationDirect: Station " + csr.StationName + ": " + err.Error())
		jsApiResp.Error = NewJSStreamCreateError(err)
		respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
		return
	}

	if exist {
		errMsg := "Station " + stationName.Ext() + " already exists"
		serv.Warnf("createStationDirect: " + errMsg)
		jsApiResp.Error = NewJSStreamNameExistError()
		respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
		return
	}

	schemaName := csr.SchemaName
	var schemaDetails models.SchemaDetails
	if schemaName != "" {
		schemaName = strings.ToLower(csr.SchemaName)
		exist, schema, err := IsSchemaExist(schemaName)
		if err != nil {
			serv.Errorf("createStationDirect: Station " + csr.StationName + ": " + err.Error())
			jsApiResp.Error = NewJSStreamCreateError(err)
			respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
			return
		}
		if !exist {
			errMsg := "Schema " + csr.SchemaName + " does not exist"
			serv.Warnf("createStationDirect: " + errMsg)
			jsApiResp.Error = NewJSStreamCreateError(err)
			respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
			return
		}

		schemaVersion, err := getActiveVersionBySchemaId(schema.ID)
		if err != nil {
			serv.Errorf("createStationDirect: Station " + csr.StationName + ": " + err.Error())
			jsApiResp.Error = NewJSStreamCreateError(err)
			respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
			return
		}
		schemaDetails = models.SchemaDetails{SchemaName: schemaName, VersionNumber: schemaVersion.VersionNumber}
	}

	var retentionType string
	var retentionValue int
	if csr.RetentionType != "" {
		retentionType = strings.ToLower(csr.RetentionType)
		err = validateRetentionType(retentionType)
		if err != nil {
			serv.Warnf("createStationDirect: " + err.Error())
			jsApiResp.Error = NewJSStreamCreateError(err)
			respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
			return
		}
		retentionValue = csr.RetentionValue
	} else {
		retentionType = "message_age_sec"
		retentionValue = 604800 // 1 week
	}

	var storageType string
	if csr.StorageType != "" {
		storageType = strings.ToLower(csr.StorageType)
		err = validateStorageType(storageType)
		if err != nil {
			serv.Warnf("createStationDirect: " + err.Error())
			jsApiResp.Error = NewJSStreamCreateError(err)
			respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
			return
		}
	} else {
		storageType = "file"
	}

	replicas := csr.Replicas
	if replicas > 0 {
		err = validateReplicas(replicas)
		if err != nil {
			serv.Warnf("createStationDirect: " + err.Error())
			jsApiResp.Error = NewJSStreamCreateError(err)
			respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
			return
		}
	} else {
		replicas = 1
	}

	if csr.IdempotencyWindow <= 0 {
		csr.IdempotencyWindow = 120000 // default
	} else if csr.IdempotencyWindow < 100 {
		csr.IdempotencyWindow = 100 // minimum is 100 millis
	}

	newStation := models.Station{
		ID:                primitive.NewObjectID(),
		Name:              stationName.Ext(),
		CreatedByUser:     c.memphisInfo.username,
		CreationDate:      time.Now(),
		IsDeleted:         false,
		RetentionType:     retentionType,
		RetentionValue:    retentionValue,
		StorageType:       storageType,
		Replicas:          replicas,
		DedupEnabled:      csr.DedupEnabled,      // TODO deprecated
		DedupWindowInMs:   csr.DedupWindowMillis, // TODO deprecated
		LastUpdate:        time.Now(),
		Schema:            schemaDetails,
		Functions:         []models.Function{},
		IdempotencyWindow: csr.IdempotencyWindow,
		IsNative:          isNative,
		DlsConfiguration:  csr.DlsConfiguration,
	}

	createStreamFunc := nonNativeCreateStreamFunc

	if createStreamFunc == nil {
		createStreamFunc = func() error {
			return s.CreateStream(stationName, newStation)
		}
	}

	err = createStreamFunc()
	if err != nil {
		serv.Errorf("createStationDirect: Station " + csr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	err = s.CreateDlsStream(stationName, newStation)
	if err != nil {
		serv.Errorf("createStationDirect: Create DLS at station " + csr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	_, err = stationsCollection.InsertOne(context.TODO(), newStation)
	if err != nil {
		serv.Errorf("createStationDirect: Station " + csr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}
	message := "Station " + stationName.Ext() + " has been created by user " + c.memphisInfo.username
	serv.Noticef(message)

	var auditLogs []interface{}
	newAuditLog := models.AuditLog{
		ID:            primitive.NewObjectID(),
		StationName:   stationName.Ext(),
		Message:       message,
		CreatedByUser: c.memphisInfo.username,
		CreationDate:  time.Now(),
		UserType:      "application",
	}
	auditLogs = append(auditLogs, newAuditLog)
	err = CreateAuditLogs(auditLogs)
	if err != nil {
		serv.Errorf("createStationDirect: Station " + csr.StationName + " - create audit logs error: " + err.Error())
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		param := analytics.EventParam{
			Name:  "station-name",
			Value: stationName.Ext(),
		}
		analyticsParams := []analytics.EventParam{param}
		analytics.SendEventWithParams(c.memphisInfo.username, analyticsParams, "user-create-station")
	}

	respondWithErr(s, reply, nil)
}

func (sh StationsHandler) GetStation(c *gin.Context) {
	var body models.GetStationSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}
	tagsHandler := TagsHandler{S: sh.S}

	var station models.GetStationResponseSchema
	err := stationsCollection.FindOne(context.TODO(), bson.M{
		"name": body.StationName,
		"$or": []interface{}{
			bson.M{"is_deleted": false},
			bson.M{"is_deleted": bson.M{"$exists": false}},
		},
	}).Decode(&station)
	if err == mongo.ErrNoDocuments {
		errMsg := "Station " + body.StationName + " does not exist"
		serv.Warnf("GetStation: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	} else if err != nil {
		serv.Errorf("GetStation: Station " + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	tags, err := tagsHandler.GetTagsByStation(station.ID)
	if err != nil {
		serv.Errorf("GetStation: Station " + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	station.Tags = tags
	if station.StorageType == "file" {
		station.StorageType = "disk"
	}

	c.IndentedJSON(200, station)
}

func (sh StationsHandler) GetStationsDetails() ([]models.ExtendedStationDetails, error) {
	var exStations []models.ExtendedStationDetails
	var stations []models.Station

	poisonMsgsHandler := PoisonMessagesHandler{S: sh.S}
	filter := bson.M{"$or": []interface{}{
		bson.M{"is_deleted": bson.M{"$exists": false}},
		bson.M{"is_deleted": false},
	}}
	cursor, err := stationsCollection.Find(context.TODO(), filter)
	if err != nil {
		return []models.ExtendedStationDetails{}, err
	}

	if err = cursor.All(context.TODO(), &stations); err != nil {
		return []models.ExtendedStationDetails{}, err
	}

	if len(stations) == 0 {
		return []models.ExtendedStationDetails{}, nil
	} else {
		tagsHandler := TagsHandler{S: sh.S}
		for _, station := range stations {
			totalMessages, err := sh.GetTotalMessages(station.Name)
			if err != nil {
				if IsNatsErr(err, JSStreamNotFoundErr) {
					continue
				} else {
					return []models.ExtendedStationDetails{}, err
				}
			}
			poisonMessages, err := poisonMsgsHandler.GetTotalPoisonMsgsByStation(station.Name)
			if err != nil {
				if IsNatsErr(err, JSStreamNotFoundErr) {
					continue
				} else {
					return []models.ExtendedStationDetails{}, err
				}
			}
			tags, err := tagsHandler.GetTagsByStation(station.ID)
			if err != nil {
				return []models.ExtendedStationDetails{}, err
			}
			if station.StorageType == "file" {
				station.StorageType = "disk"
			}
			exStations = append(exStations, models.ExtendedStationDetails{Station: station, PoisonMessages: poisonMessages, TotalMessages: totalMessages, Tags: tags})
		}
		if exStations == nil {
			return []models.ExtendedStationDetails{}, nil
		}
		return exStations, nil
	}
}

func (sh StationsHandler) GetAllStationsDetails() ([]models.ExtendedStation, error) {
	var stations []models.ExtendedStation
	cursor, err := stationsCollection.Aggregate(context.TODO(), mongo.Pipeline{
		bson.D{{"$match", bson.D{{"$or", []interface{}{
			bson.D{{"is_deleted", false}},
			bson.D{{"is_deleted", bson.D{{"$exists", false}}}},
		}}}}},
		bson.D{{"$project", bson.D{{"_id", 1}, {"name", 1}, {"retention_type", 1}, {"retention_value", 1}, {"storage_type", 1}, {"replicas", 1}, {"idempotency_window_in_ms", 1}, {"created_by_user", 1}, {"creation_date", 1}, {"last_update", 1}, {"functions", 1}, {"dls_configuration", 1}}}},
	})
	if err != nil {
		return stations, err
	}

	if err = cursor.All(context.TODO(), &stations); err != nil {
		return stations, err
	}

	if len(stations) == 0 {
		return []models.ExtendedStation{}, nil
	} else {
		poisonMsgsHandler := PoisonMessagesHandler{S: sh.S}
		tagsHandler := TagsHandler{S: sh.S}
		var extStations []models.ExtendedStation
		for i := 0; i < len(stations); i++ {
			totalMessages, err := sh.GetTotalMessages(stations[i].Name)
			if err != nil {
				if IsNatsErr(err, JSStreamNotFoundErr) {
					continue
				} else {
					return []models.ExtendedStation{}, err
				}
			}
			poisonMessages, err := poisonMsgsHandler.GetTotalPoisonMsgsByStation(stations[i].Name)
			if err != nil {
				if IsNatsErr(err, JSStreamNotFoundErr) {
					continue
				} else {
					return []models.ExtendedStation{}, err
				}
			}
			tags, err := tagsHandler.GetTagsByStation(stations[i].ID)
			if err != nil {
				return []models.ExtendedStation{}, err
			}

			stations[i].TotalMessages = totalMessages
			stations[i].PoisonMessages = poisonMessages
			stations[i].Tags = tags
			extStations = append(extStations, stations[i])
		}
		return extStations, nil
	}
}

func (sh StationsHandler) GetStations(c *gin.Context) {
	stations, err := sh.GetStationsDetails()
	if err != nil {
		serv.Errorf("GetStations: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	c.IndentedJSON(200, gin.H{
		"stations": stations,
	})
}

func (sh StationsHandler) GetAllStations(c *gin.Context) {
	stations, err := sh.GetAllStationsDetails()
	if err != nil {
		serv.Errorf("GetAllStations: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	c.IndentedJSON(200, stations)
}

func (sh StationsHandler) CreateStation(c *gin.Context) {
	var body models.CreateStationSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	stationName, err := StationNameFromStr(body.Name)
	if err != nil {
		serv.Warnf("CreateStation: Station " + body.Name + ": " + err.Error())
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
		return
	}

	exist, _, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	if exist {
		errMsg := "Station " + stationName.external + " already exists"
		serv.Warnf("CreateStation: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}

	user, err := getUserDetailsFromMiddleware(c)
	if err != nil {
		serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
		c.AbortWithStatusJSON(401, gin.H{"message": "Unauthorized"})
	}

	schemaName := body.SchemaName
	var schemaDetails models.SchemaDetails
	var schemaDetailsResponse models.StationOverviewSchemaDetails
	if schemaName != "" {
		schemaName = strings.ToLower(body.SchemaName)
		exist, schema, err := IsSchemaExist(schemaName)
		if err != nil {
			serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server Error"})
			return
		}
		if !exist {
			errMsg := "Schema " + schemaName + " does not exist"
			serv.Warnf("CreateStation: Station " + body.Name + ": " + errMsg)
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
			return
		}

		schemaVersion, err := getActiveVersionBySchemaId(schema.ID)
		if err != nil {
			serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": err.Error()})
			return
		}

		schemaDetailsResponse = models.StationOverviewSchemaDetails{SchemaName: schemaName, VersionNumber: schemaVersion.VersionNumber, UpdatesAvailable: true}
		schemaDetails = models.SchemaDetails{SchemaName: schemaName, VersionNumber: schemaVersion.VersionNumber}
	}

	var retentionType string
	if body.RetentionType != "" && body.RetentionValue > 0 {
		retentionType = strings.ToLower(body.RetentionType)
		err = validateRetentionType(retentionType)
		if err != nil {
			serv.Warnf("CreateStation: Station " + body.Name + ": " + err.Error())
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
			return
		}
	} else {
		retentionType = "message_age_sec"
		body.RetentionValue = 604800 // 1 week
	}

	if body.StorageType != "" {
		body.StorageType = strings.ToLower(body.StorageType)
		err = validateStorageType(body.StorageType)
		if err != nil {
			serv.Warnf("CreateStation: Station " + body.Name + ": " + err.Error())
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
			return
		}
	} else {
		body.StorageType = "file"
	}

	storageTypeForResponse := "disk"
	if body.StorageType == "memory" {
		storageTypeForResponse = body.StorageType
	}

	if body.Replicas > 0 {
		err = validateReplicas(body.Replicas)
		if err != nil {
			serv.Warnf("CreateStation: Station " + body.Name + ": " + err.Error())
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
			return
		}
	} else {
		body.Replicas = 1
	}

	if body.IdempotencyWindow <= 0 {
		body.IdempotencyWindow = 120000 // default
	} else if body.IdempotencyWindow < 100 {
		body.IdempotencyWindow = 100 // minimum is 100 millis
	}

	newStation := models.Station{
		ID:                primitive.NewObjectID(),
		Name:              stationName.Ext(),
		RetentionType:     retentionType,
		RetentionValue:    body.RetentionValue,
		StorageType:       body.StorageType,
		Replicas:          body.Replicas,
		DedupEnabled:      body.DedupEnabled,    // TODO deprecated
		DedupWindowInMs:   body.DedupWindowInMs, // TODO deprecated
		CreatedByUser:     user.Username,
		CreationDate:      time.Now(),
		LastUpdate:        time.Now(),
		Functions:         []models.Function{},
		IsDeleted:         false,
		Schema:            schemaDetails,
		IdempotencyWindow: body.IdempotencyWindow,
		DlsConfiguration:  body.DlsConfiguration,
		IsNative:          true,
	}

	err = sh.S.CreateStream(stationName, newStation)
	if err != nil {
		serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	err = sh.S.CreateDlsStream(stationName, newStation)
	if err != nil {
		serv.Errorf("CreateStation: Create DLS at station " + body.Name + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	var emptySchemaDetailsResponse struct{}
	var update bson.M
	filter := bson.M{"name": newStation.Name, "is_deleted": false}
	if schemaName != "" {
		update = bson.M{
			"$setOnInsert": bson.M{
				"_id":                      newStation.ID,
				"retention_type":           newStation.RetentionType,
				"retention_value":          newStation.RetentionValue,
				"storage_type":             newStation.StorageType,
				"replicas":                 newStation.Replicas,
				"dedup_enabled":            newStation.DedupEnabled,    // TODO deprecated
				"dedup_window_in_ms":       newStation.DedupWindowInMs, // TODO deprecated
				"created_by_user":          newStation.CreatedByUser,
				"creation_date":            newStation.CreationDate,
				"last_update":              newStation.LastUpdate,
				"functions":                newStation.Functions,
				"schema":                   newStation.Schema,
				"idempotency_window_in_ms": newStation.IdempotencyWindow,
				"dls_configuration":        newStation.DlsConfiguration,
				"is_native":                newStation.IsNative,
			},
		}
	} else {
		update = bson.M{
			"$setOnInsert": bson.M{
				"_id":                      newStation.ID,
				"retention_type":           newStation.RetentionType,
				"retention_value":          newStation.RetentionValue,
				"storage_type":             newStation.StorageType,
				"replicas":                 newStation.Replicas,
				"dedup_enabled":            newStation.DedupEnabled,    // TODO deprecated
				"dedup_window_in_ms":       newStation.DedupWindowInMs, // TODO deprecated
				"created_by_user":          newStation.CreatedByUser,
				"creation_date":            newStation.CreationDate,
				"last_update":              newStation.LastUpdate,
				"functions":                newStation.Functions,
				"schema":                   emptySchemaDetailsResponse,
				"idempotency_window_in_ms": newStation.IdempotencyWindow,
				"dls_configuration":        newStation.DlsConfiguration,
				"is_native":                newStation.IsNative,
			},
		}
	}
	opts := options.Update().SetUpsert(true)
	updateResults, err := stationsCollection.UpdateOne(context.TODO(), filter, update, opts)
	if err != nil {
		serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	if updateResults.MatchedCount > 0 {
		errMsg := "Station " + newStation.Name + " already exists"
		serv.Warnf("CreateStation: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}

	if len(body.Tags) > 0 {
		err = AddTagsToEntity(body.Tags, "station", newStation.ID)
		if err != nil {
			serv.Errorf("CreateStation: : Station " + body.Name + " Failed adding tags: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
	}

	message := "Station " + stationName.Ext() + " has been created by " + user.Username
	serv.Noticef(message)
	var auditLogs []interface{}
	newAuditLog := models.AuditLog{
		ID:            primitive.NewObjectID(),
		StationName:   stationName.Ext(),
		Message:       message,
		CreatedByUser: user.Username,
		CreationDate:  time.Now(),
		UserType:      user.UserType,
	}
	auditLogs = append(auditLogs, newAuditLog)
	err = CreateAuditLogs(auditLogs)
	if err != nil {
		serv.Errorf("CreateStation: Station " + body.Name + ": " + err.Error())
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		param := analytics.EventParam{
			Name:  "station-name",
			Value: stationName.Ext(),
		}
		analyticsParams := []analytics.EventParam{param}
		analytics.SendEventWithParams(user.Username, analyticsParams, "user-create-station")
	}

	if schemaName != "" {
		c.IndentedJSON(200, gin.H{
			"id":                       primitive.NewObjectID(),
			"name":                     stationName.Ext(),
			"retention_type":           retentionType,
			"retention_value":          body.RetentionValue,
			"storage_type":             storageTypeForResponse,
			"replicas":                 body.Replicas,
			"dedup_enabled":            body.DedupEnabled,    // TODO deprecated
			"dedup_window_in_ms":       body.DedupWindowInMs, // TODO deprecated
			"created_by_user":          user.Username,
			"creation_date":            time.Now(),
			"last_update":              time.Now(),
			"functions":                []models.Function{},
			"is_deleted":               false,
			"schema":                   schemaDetailsResponse,
			"idempotency_window_in_ms": newStation.IdempotencyWindow,
			"dls_configuration":        newStation.DlsConfiguration,
		})
	} else {
		c.IndentedJSON(200, gin.H{
			"id":                       primitive.NewObjectID(),
			"name":                     stationName.Ext(),
			"retention_type":           retentionType,
			"retention_value":          body.RetentionValue,
			"storage_type":             storageTypeForResponse,
			"replicas":                 body.Replicas,
			"dedup_enabled":            body.DedupEnabled,    // TODO deprecated
			"dedup_window_in_ms":       body.DedupWindowInMs, // TODO deprecated
			"created_by_user":          user.Username,
			"creation_date":            time.Now(),
			"last_update":              time.Now(),
			"functions":                []models.Function{},
			"is_deleted":               false,
			"schema":                   emptySchemaDetailsResponse,
			"idempotency_window_in_ms": newStation.IdempotencyWindow,
			"dls_configuration":        newStation.DlsConfiguration,
		})
	}
}

func (sh StationsHandler) RemoveStation(c *gin.Context) {
	if err := DenyForSandboxEnv(c); err != nil {
		return
	}
	var body models.RemoveStationSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	var stationNames []string
	for _, name := range body.StationNames {
		stationName, err := StationNameFromStr(name)
		if err != nil {
			serv.Warnf("RemoveStation: Station " + name + ": " + err.Error())
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
			return
		}

		stationNames = append(stationNames, stationName.Ext())

		exist, station, err := IsStationExist(stationName)
		if err != nil {
			serv.Errorf("RemoveStation: Station " + stationName.external + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
		if !exist {
			errMsg := "Station " + name + " does not exist"
			serv.Warnf("RemoveStation: " + errMsg)
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
			return
		}

		err = removeStationResources(sh.S, station, nil)
		if err != nil {
			serv.Errorf("RemoveStation: Station " + stationName.external + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
	}

	_, err := stationsCollection.UpdateMany(context.TODO(),
		bson.M{
			"name": bson.M{"$in": stationNames},
			"$or": []interface{}{
				bson.M{"is_deleted": false},
				bson.M{"is_deleted": bson.M{"$exists": false}},
			},
		},
		bson.M{"$set": bson.M{"is_deleted": true}},
	)
	if err != nil {
		serv.Errorf("RemoveStation: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-remove-station")
	}

	for _, name := range body.StationNames {
		stationName, err := StationNameFromStr(name)
		if err != nil {
			serv.Errorf("RemoveStation: Station " + name + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		}

		user, err := getUserDetailsFromMiddleware(c)
		if err != nil {
			serv.Errorf("RemoveStation: Station " + name + ": " + err.Error())
			c.AbortWithStatusJSON(401, gin.H{"message": "Unauthorized"})
		}

		message := "Station " + stationName.Ext() + " has been deleted by user " + user.Username
		serv.Noticef(message)

		var auditLogs []interface{}
		newAuditLog := models.AuditLog{
			ID:            primitive.NewObjectID(),
			StationName:   stationName.Ext(),
			Message:       message,
			CreatedByUser: user.Username,
			CreationDate:  time.Now(),
			UserType:      user.UserType,
		}
		auditLogs = append(auditLogs, newAuditLog)
		err = CreateAuditLogs(auditLogs)
		if err != nil {
			serv.Warnf("RemoveStation: Station " + name + " - create audit logs error: " + err.Error())
		}
	}
	c.IndentedJSON(200, gin.H{})
}

func (s *Server) removeStationDirect(c *client, reply string, msg []byte) {
	var dsr destroyStationRequest
	if err := json.Unmarshal(msg, &dsr); err != nil {
		s.Errorf("removeStationDirect: " + err.Error())
		respondWithErr(s, reply, err)
		return
	}
	s.removeStationDirectIntern(c, reply, &dsr, nil)
}

func (s *Server) removeStationDirectIntern(c *client,
	reply string,
	dsr *destroyStationRequest,
	nonNativeRemoveStreamFunc func() error) {
	isNative := nonNativeRemoveStreamFunc == nil
	jsApiResp := JSApiStreamDeleteResponse{ApiResponse: ApiResponse{Type: JSApiStreamDeleteResponseType}}

	stationName, err := StationNameFromStr(dsr.StationName)
	if err != nil {
		serv.Warnf("removeStationDirect: Station " + dsr.StationName + ": " + err.Error())
		jsApiResp.Error = NewJSStreamDeleteError(err)
		respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
		return
	}

	exist, station, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("removeStationDirect: Station " + dsr.StationName + ": " + err.Error())
		jsApiResp.Error = NewJSStreamDeleteError(err)
		respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
		return
	}
	if !exist {
		errMsg := "Station " + station.Name + " does not exist"
		serv.Warnf("removeStationDirect: " + errMsg)
		err := errors.New(errMsg)
		jsApiResp.Error = NewJSStreamDeleteError(err)
		respondWithErrOrJsApiResp(!isNative, c, c.acc, _EMPTY_, reply, _EMPTY_, jsApiResp, err)
		return
	}

	err = removeStationResources(s, station, nonNativeRemoveStreamFunc)
	if err != nil {
		serv.Errorf("RemoveStation: Station " + dsr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	_, err = stationsCollection.UpdateOne(context.TODO(),
		bson.M{
			"name": stationName.Ext(),
			"$or": []interface{}{
				bson.M{"is_deleted": false},
				bson.M{"is_deleted": bson.M{"$exists": false}},
			},
		},
		bson.M{"$set": bson.M{"is_deleted": true}},
	)
	if err != nil {
		serv.Errorf("RemoveStation error: Station " + dsr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	message := "Station " + stationName.Ext() + " has been deleted by user " + c.memphisInfo.username
	serv.Noticef(message)
	var auditLogs []interface{}
	newAuditLog := models.AuditLog{
		ID:            primitive.NewObjectID(),
		StationName:   stationName.Ext(),
		Message:       message,
		CreatedByUser: c.memphisInfo.username,
		CreationDate:  time.Now(),
		UserType:      "application",
	}
	auditLogs = append(auditLogs, newAuditLog)
	err = CreateAuditLogs(auditLogs)
	if err != nil {
		serv.Warnf("removeStationDirect: Station " + stationName.Ext() + " - create audit logs error: " + err.Error())
	}
	respondWithErr(s, reply, nil)
	return
}

func (sh StationsHandler) GetTotalMessages(stationNameExt string) (int, error) {
	stationName, err := StationNameFromStr(stationNameExt)
	if err != nil {
		return 0, err
	}
	totalMessages, err := sh.S.GetTotalMessagesInStation(stationName)
	return totalMessages, err
}

func (sh StationsHandler) GetTotalMessagesAcrossAllStations() (int, error) {
	totalMessages, err := sh.S.GetTotalMessagesAcrossAllStations()
	return totalMessages, err
}

func (sh StationsHandler) GetAvgMsgSize(station models.Station) (int64, error) {
	avgMsgSize, err := sh.S.GetAvgMsgSizeInStation(station)
	return avgMsgSize, err
}

func (sh StationsHandler) GetMessages(station models.Station, messagesToFetch int) ([]models.MessageDetails, error) {
	messages, err := sh.S.GetMessages(station, messagesToFetch)
	if err != nil {
		return messages, err
	}

	return messages, nil
}

func (sh StationsHandler) GetLeaderAndFollowers(station models.Station) (string, []string, error) {
	if sh.S.JetStreamIsClustered() {
		leader, followers, err := sh.S.GetLeaderAndFollowers(station)
		if err != nil {
			return "", []string{}, err
		}

		return leader, followers, nil
	} else {
		return "broker-0", []string{}, nil
	}
}

func getCgStatus(members []models.CgMember) (bool, bool) {
	deletedCount := 0
	for _, member := range members {
		if member.IsActive {
			return true, false
		}

		if member.IsDeleted {
			deletedCount++
		}
	}

	if len(members) == deletedCount {
		return false, true
	}

	return false, false
}

func (sh StationsHandler) GetDlsMessageJourneyDetails(dlsMsgId string) (models.DlsMessageResponse, error) {
	dlsMsgId = strings.ReplaceAll(dlsMsgId, " ", "+")
	poisonMsgsHandler := PoisonMessagesHandler{S: sh.S}
	var dlsMessage models.DlsMessageResponse
	splitId := strings.Split(dlsMsgId, dlsMsgSep)
	stationName := splitId[0]
	sn, err := StationNameFromStr(stationName)
	if err != nil {
		return dlsMessage, err
	}
	exist, station, err := IsStationExist(sn)
	if err != nil {
		return dlsMessage, err
	}
	if !exist {
		return dlsMessage, errors.New("Station " + station.Name + " does not exist")
	}

	poisoned, schemaFailed, err := poisonMsgsHandler.GetDlsMsgsByStationFull(station)
	if err != nil {
		return dlsMessage, err
	}
	for _, dlm := range poisoned {
		if dlm.ID == dlsMsgId {
			dlsMessage = dlm
			headersJson := dlsMessage.Message.Headers
			for header := range headersJson {
				if strings.HasPrefix(header, "$memphis") {
					delete(headersJson, header)
				}
			}
			dlsMessage.Message.Headers = headersJson
			break
		}
	}

	if dlsMessage.ID == "" {
		for _, dlm := range schemaFailed {
			if dlm.ID == dlsMsgId {
				dlsMessage = dlm
				break
			}
		}
	}
	seq, err := strconv.Atoi(splitId[2])
	if err != nil {
		return dlsMessage, err
	}
	poisonedCgs, err := GetPoisonedCgsByMessage(sn.Intern(), models.MessageDetails{MessageSeq: seq, ProducedBy: dlsMessage.Producer.Name, TimeSent: dlsMessage.Message.TimeSent})
	if err != nil {
		return dlsMessage, err
	}

	cgs := make([]models.PoisonedCg, 0)
	sort.Slice(poisonedCgs, func(i, j int) bool {
		return poisonedCgs[i].PoisoningTime.After(poisonedCgs[j].PoisoningTime)
	})
	cgCheck := make(map[string]bool)
	for _, cg := range poisonedCgs {
		if _, value := cgCheck[cg.CgName]; value {
			continue
		}
		cgCheck[cg.CgName] = true
		cgMembers, err := GetConsumerGroupMembers(cg.CgName, station)
		if err != nil {
			return dlsMessage, err
		}

		isActive, isDeleted := getCgStatus(cgMembers)
		cgInfo, err := sh.S.GetCgInfo(sn, cg.CgName)
		if err != nil {
			return dlsMessage, err
		}
		totalPms, err := GetTotalPoisonMsgsByCg(sn.Intern(), cg.CgName)
		if err != nil {
			return dlsMessage, err
		}
		cg.MaxAckTimeMs = cgMembers[0].MaxAckTimeMs
		cg.MaxMsgDeliveries = cgMembers[0].MaxMsgDeliveries
		cg.UnprocessedMessages = int(cgInfo.NumPending)
		cg.InProcessMessages = cgInfo.NumAckPending
		cg.TotalPoisonMessages = totalPms
		cg.CgMembers = cgMembers
		cg.IsActive = isActive
		cg.IsDeleted = isDeleted
		cgs = append(cgs, cg)
	}

	dlsMessage.PoisonedCgs = cgs

	return dlsMessage, nil
}

func (sh StationsHandler) GetPoisonMessageJourney(c *gin.Context) {
	var body models.GetPoisonMessageJourneySchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	poisonMessage, err := sh.GetDlsMessageJourneyDetails(body.MessageId)
	if err != nil {
		serv.Errorf("GetPoisonMessageJourney: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-enter-message-journey")
	}

	c.IndentedJSON(200, poisonMessage)
}

func (sh StationsHandler) AckPoisonMessages(c *gin.Context) {
	var body models.AckPoisonMessagesSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}
	timeout := 1 * time.Second
	splitId := strings.Split(body.PoisonMessageIds[0], dlsMsgSep)
	stationName := splitId[0]
	sn, err := StationNameFromStr(stationName)
	if err != nil {
		serv.Errorf("AckPoisonMessages: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	streamName := fmt.Sprintf(dlsStreamName, sn.Intern())
	for _, msgId := range body.PoisonMessageIds {
		uid := serv.memphis.nuid.Next()
		durableName := "$memphis_fetch_dls_consumer_" + uid
		var msgs []StoredMsg
		streamInfo, err := serv.memphisStreamInfo(streamName)
		if err != nil {
			serv.Errorf("AckPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
		filter := GetDlsSubject("poison", sn.Intern(), msgId)
		amount := streamInfo.State.Msgs
		cc := ConsumerConfig{
			DeliverPolicy: DeliverAll,
			AckPolicy:     AckExplicit,
			Durable:       durableName,
			FilterSubject: filter,
		}

		err = serv.memphisAddConsumer(streamName, &cc)
		if err != nil {
			serv.Errorf("AckPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		responseChan := make(chan StoredMsg)
		subject := fmt.Sprintf(JSApiRequestNextT, streamName, durableName)
		reply := durableName + "_reply"
		req := []byte(strconv.FormatUint(amount, 10))

		sub, err := serv.subscribeOnGlobalAcc(reply, reply+"_sid", func(_ *client, subject, reply string, msg []byte) {
			go func(respCh chan StoredMsg, subject, reply string, msg []byte) {
				// ack
				serv.sendInternalAccountMsg(serv.GlobalAccount(), reply, []byte(_EMPTY_))
				rawTs := tokenAt(reply, 8)
				seq, _, _ := ackReplyInfo(reply)

				intTs, err := strconv.Atoi(rawTs)
				if err != nil {
					serv.Errorf("GetTotalPoisonMsgsByCg: " + err.Error())
				}

				respCh <- StoredMsg{
					Subject:  subject,
					Sequence: uint64(seq),
					Data:     msg,
					Time:     time.Unix(0, int64(intTs)),
				}
			}(responseChan, subject, reply, copyBytes(msg))
		})
		if err != nil {
			serv.Errorf("AckPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		serv.sendInternalAccountMsgWithReply(serv.GlobalAccount(), subject, reply, nil, req, true)

		timer := time.NewTimer(timeout)
		for i := uint64(0); i < amount; i++ {
			select {
			case <-timer.C:
				goto cleanup
			case msg := <-responseChan:
				msgs = append(msgs, msg)
			}
		}

	cleanup:
		timer.Stop()
		serv.unsubscribeOnGlobalAcc(sub)
		err = serv.memphisRemoveConsumer(streamName, durableName)
		if err != nil {
			serv.Errorf("AckPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		for _, msg := range msgs {
			_, err = sh.S.memphisDeleteMsgFromStream(streamName, msg.Sequence)
			if err != nil {
				serv.Errorf("AckPoisonMessages: " + err.Error())
				c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
				return
			}
		}
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-ack-poison-message")
	}

	c.IndentedJSON(200, gin.H{})
}

func (sh StationsHandler) ResendPoisonMessages(c *gin.Context) {
	var body models.ResendPoisonMessagesSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}
	timeout := 1 * time.Second
	splitId := strings.Split(body.PoisonMessageIds[0], dlsMsgSep)
	stationName := splitId[0]
	sn, err := StationNameFromStr(stationName)
	if err != nil {
		serv.Errorf("ResendPoisonMessages: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	streamName := fmt.Sprintf(dlsStreamName, sn.Intern())
	for _, msgId := range body.PoisonMessageIds {
		uid := serv.memphis.nuid.Next()
		durableName := "$memphis_fetch_dls_consumer_" + uid
		var msgs []StoredMsg
		streamInfo, err := serv.memphisStreamInfo(streamName)
		if err != nil {
			serv.Errorf("ResendPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
		filter := GetDlsSubject("poison", sn.Intern(), msgId)
		amount := streamInfo.State.Msgs
		cc := ConsumerConfig{
			DeliverPolicy: DeliverAll,
			AckPolicy:     AckExplicit,
			Durable:       durableName,
			FilterSubject: filter,
		}

		err = serv.memphisAddConsumer(streamName, &cc)
		if err != nil {
			serv.Errorf("ResendPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		responseChan := make(chan StoredMsg)
		subject := fmt.Sprintf(JSApiRequestNextT, streamName, durableName)
		reply := durableName + "_reply"
		req := []byte(strconv.FormatUint(amount, 10))

		sub, err := serv.subscribeOnGlobalAcc(reply, reply+"_sid", func(_ *client, subject, reply string, msg []byte) {
			go func(respCh chan StoredMsg, subject, reply string, msg []byte) {
				// ack
				serv.sendInternalAccountMsg(serv.GlobalAccount(), reply, []byte(_EMPTY_))
				rawTs := tokenAt(reply, 8)
				seq, _, _ := ackReplyInfo(reply)

				intTs, err := strconv.Atoi(rawTs)
				if err != nil {
					serv.Errorf("ResendPoisonMessages: " + err.Error())
				}

				respCh <- StoredMsg{
					Subject:  subject,
					Sequence: uint64(seq),
					Data:     msg,
					Time:     time.Unix(0, int64(intTs)),
				}
			}(responseChan, subject, reply, copyBytes(msg))
		})
		if err != nil {
			serv.Errorf("ResendPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		serv.sendInternalAccountMsgWithReply(serv.GlobalAccount(), subject, reply, nil, req, true)

		timer := time.NewTimer(timeout)
		for i := uint64(0); i < amount; i++ {
			select {
			case <-timer.C:
				goto cleanup
			case msg := <-responseChan:
				msgs = append(msgs, msg)
			}
		}

	cleanup:
		timer.Stop()
		serv.unsubscribeOnGlobalAcc(sub)
		err = serv.memphisRemoveConsumer(streamName, durableName)
		if err != nil {
			serv.Errorf("ResendPoisonMessages: " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		for _, msg := range msgs {
			var dlsMsg models.DlsMessage
			err = json.Unmarshal(msg.Data, &dlsMsg)
			if err != nil {
				serv.Errorf("ResendPoisonMessages: " + err.Error())
				c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
				return
			}
			stationName := replaceDelimiters(dlsMsg.StationName)
			cgName := replaceDelimiters(dlsMsg.PoisonedCg.CgName)
			headersJson := map[string]string{}
			for key, value := range dlsMsg.Message.Headers {
				headersJson[key] = value
			}
			headersJson["$memphis_pm_id"] = dlsMsg.ID
			headersJson["$memphis_pm_sequence"] = strconv.FormatUint(msg.Sequence, 10)
			headers, err := json.Marshal(headersJson)
			if err != nil {
				serv.Errorf("ResendPoisonMessages: Poisoned consumer group: " + dlsMsg.PoisonedCg.CgName + ": " + err.Error())
				c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
				return
			}
			data, err := hex.DecodeString(dlsMsg.Message.Data)
			if err != nil {
				serv.Errorf("ResendPoisonMessages: Poisoned consumer group: " + dlsMsg.PoisonedCg.CgName + ": " + err.Error())
				c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
				return
			}
			err = sh.S.ResendPoisonMessage("$memphis_dls_"+stationName+"_"+cgName, []byte(data), headers)
			if err != nil {
				serv.Errorf("ResendPoisonMessages: Poisoned consumer group: " + dlsMsg.PoisonedCg.CgName + ": " + err.Error())
				c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
				return
			}
		}
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-resend-poison-message")
	}

	c.IndentedJSON(200, gin.H{})
}

func (sh StationsHandler) GetMessageDetails(c *gin.Context) {
	var body models.GetMessageDetailsSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	if body.IsPoisonMessage {
		poisonMessage, err := sh.GetDlsMessageJourneyDetails(body.MessageId)
		if err != nil {
			serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		c.IndentedJSON(200, poisonMessage)
		return
	}

	stationName, err := StationNameFromStr(body.StationName)
	if err != nil {
		serv.Warnf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
		return
	}

	exist, station, err := IsStationExist(stationName)
	if !exist {
		errMsg := "Station " + stationName.external + " does not exist"
		serv.Warnf("GetMessageDetails: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}
	if err != nil {
		serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	sm, err := sh.S.GetMessage(stationName, uint64(body.MessageSeq))
	if err != nil {
		serv.Errorf("GetMessageDetails: Message ID: Message ID: " + body.MessageId + ": " + body.MessageId + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	if !station.IsNative {
		msg := models.MessageResponse{
			MessageSeq: body.MessageSeq,
			Message: models.MessagePayload{
				TimeSent: sm.Time,
				Size:     len(sm.Subject) + len(sm.Data) + len(sm.Header),
				Data:     hex.EncodeToString(sm.Data),
				Headers:  map[string]string{},
			},
			Producer: models.ProducerDetails{
				Name:          "",
				ConnectionId:  primitive.ObjectID{},
				ClientAddress: "",
				CreatedByUser: "",
				IsActive:      false,
				IsDeleted:     false,
			},
			PoisonedCgs: []models.PoisonedCg{},
		}
		c.IndentedJSON(200, msg)
		return
	}
	headersJson, err := DecodeHeader(sm.Header)
	if err != nil {
		serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	connectionIdHeader := headersJson["$memphis_connectionId"]
	producedByHeader := strings.ToLower(headersJson["$memphis_producedBy"])

	//This check for backward compatability
	if connectionIdHeader == "" || producedByHeader == "" {
		connectionIdHeader = headersJson["connectionId"]
		producedByHeader = strings.ToLower(headersJson["producedBy"])
		if connectionIdHeader == "" || producedByHeader == "" {
			serv.Warnf("GetMessageDetails: Error while getting notified about a poison message: Missing mandatory message headers, please upgrade the SDK version you are using")
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": "Error while getting notified about a poison message: Missing mandatory message headers, please upgrade the SDK version you are using"})
			return
		}
	}

	for header := range headersJson {
		if strings.HasPrefix(header, "$memphis") {
			delete(headersJson, header)
		}
	}

	connectionId, _ := primitive.ObjectIDFromHex(connectionIdHeader)
	poisonedCgs, err := GetPoisonedCgsByMessage(stationName.Intern(), models.MessageDetails{MessageSeq: int(sm.Sequence), ProducedBy: producedByHeader, TimeSent: sm.Time})
	if err != nil {
		serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	for i, cg := range poisonedCgs {
		cgInfo, err := sh.S.GetCgInfo(stationName, cg.CgName)
		if err != nil {
			serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		totalPoisonMsgs, err := GetTotalPoisonMsgsByCg(stationName.Ext(), cg.CgName)
		if err != nil {
			serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		cgMembers, err := GetConsumerGroupMembers(cg.CgName, station)
		if err != nil {
			serv.Errorf("GetMessageDetails: Message ID: " + body.MessageId + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}

		isActive, isDeleted := getCgStatus(cgMembers)

		poisonedCgs[i].MaxAckTimeMs = cgMembers[0].MaxAckTimeMs
		poisonedCgs[i].MaxMsgDeliveries = cgMembers[0].MaxMsgDeliveries
		poisonedCgs[i].UnprocessedMessages = int(cgInfo.NumPending)
		poisonedCgs[i].InProcessMessages = cgInfo.NumAckPending
		poisonedCgs[i].TotalPoisonMessages = totalPoisonMsgs
		poisonedCgs[i].IsActive = isActive
		poisonedCgs[i].IsDeleted = isDeleted
	}

	filter := bson.M{"name": producedByHeader, "station_id": station.ID, "connection_id": connectionId}
	var producer models.Producer
	err = producersCollection.FindOne(context.TODO(), filter).Decode(&producer)
	if err != nil {
		serv.Errorf("GetMessageDetails: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	_, conn, err := IsConnectionExist(connectionId)
	if err != nil {
		serv.Errorf("GetMessageDetails: " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	sort.Slice(poisonedCgs, func(i, j int) bool {
		return poisonedCgs[i].PoisoningTime.After(poisonedCgs[j].PoisoningTime)
	})

	msg := models.MessageResponse{
		MessageSeq: body.MessageSeq,
		Message: models.MessagePayload{
			TimeSent: sm.Time,
			Size:     len(sm.Subject) + len(sm.Data) + len(sm.Header),
			Data:     hex.EncodeToString(sm.Data),
			Headers:  headersJson,
		},
		Producer: models.ProducerDetails{
			Name:          producedByHeader,
			ConnectionId:  connectionId,
			ClientAddress: conn.ClientAddress,
			CreatedByUser: producer.CreatedByUser,
			IsActive:      producer.IsActive,
			IsDeleted:     producer.IsDeleted,
		},
		PoisonedCgs: poisonedCgs,
	}
	c.IndentedJSON(200, msg)
}

func (sh StationsHandler) UseSchema(c *gin.Context) {
	var body models.UseSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	schemaName := strings.ToLower(body.SchemaName)
	exist, schema, err := IsSchemaExist(schemaName)
	if err != nil {
		serv.Errorf("UseSchema: Schema " + body.SchemaName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server Error"})
		return
	}
	if !exist {
		errMsg := "Schema " + schemaName + " does not exist"
		serv.Warnf("UseSchema: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}

	schemaVersion, err := getActiveVersionBySchemaId(schema.ID)
	if err != nil {
		serv.Errorf("UseSchema: Schema " + body.SchemaName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": err.Error()})
		return
	}
	schemaDetailsResponse := models.StationOverviewSchemaDetails{SchemaName: schemaName, VersionNumber: schemaVersion.VersionNumber, UpdatesAvailable: false}
	schemaDetails := models.SchemaDetails{SchemaName: schemaName, VersionNumber: schemaVersion.VersionNumber}

	user, err := getUserDetailsFromMiddleware(c)
	if err != nil {
		serv.Errorf("UseSchema: Schema " + body.SchemaName + ": " + err.Error())
		c.AbortWithStatusJSON(401, gin.H{"message": "Unauthorized"})
	}

	for _, stationName := range body.StationNames {
		stationName, err := StationNameFromStr(stationName)
		if err != nil {
			serv.Warnf("UseSchema: Schema " + body.SchemaName + " at station " + stationName.Ext() + ": " + err.Error())
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
			return
		}

		exist, station, err := IsStationExist(stationName)
		if err != nil {
			serv.Errorf("UseSchema: Schema " + body.SchemaName + " at station " + stationName.Ext() + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
		if !exist {
			errMsg := "Station " + station.Name + " does not exist"
			serv.Warnf("UseSchema: Schema " + body.SchemaName + ": " + errMsg)
			c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
			return
		}

		_, err = stationsCollection.UpdateOne(context.TODO(), bson.M{"name": stationName.Ext(), "is_deleted": false}, bson.M{"$set": bson.M{"schema": schemaDetails}})
		if err != nil {
			serv.Errorf("UseSchema: Schema " + body.SchemaName + " at station " + stationName.Ext() + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": err.Error()})
			return
		}

		message := "Schema " + schemaName + " has been attached to station " + stationName.Ext() + " by user " + user.Username
		serv.Noticef(message)

		var auditLogs []interface{}
		newAuditLog := models.AuditLog{
			ID:            primitive.NewObjectID(),
			StationName:   stationName.Intern(),
			Message:       message,
			CreatedByUser: user.Username,
			CreationDate:  time.Now(),
			UserType:      user.UserType,
		}
		auditLogs = append(auditLogs, newAuditLog)
		err = CreateAuditLogs(auditLogs)
		if err != nil {
			serv.Errorf("UseSchema: Schema " + body.SchemaName + " at station " + stationName.Ext() + " - create audit logs: " + err.Error())
		}

		updateContent, err := generateSchemaUpdateInit(schema)
		if err != nil {
			serv.Errorf("UseSchema: Schema " + body.SchemaName + " at station " + stationName.Ext() + ": " + err.Error())
			return
		}
		update := models.ProducerSchemaUpdate{
			UpdateType: models.SchemaUpdateTypeInit,
			Init:       *updateContent,
		}
		sh.S.updateStationProducersOfSchemaChange(stationName, update)
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-attach-schema-to-station")
	}

	c.IndentedJSON(200, schemaDetailsResponse)
}

func (s *Server) useSchemaDirect(c *client, reply string, msg []byte) {
	var asr attachSchemaRequest
	if err := json.Unmarshal(msg, &asr); err != nil {
		errMsg := "failed attaching schema " + asr.Name + ": " + err.Error()
		s.Errorf("useSchemaDirect: At station " + asr.StationName + " " + errMsg)
		respondWithErr(s, reply, errors.New(errMsg))
		return
	}
	stationName, err := StationNameFromStr(asr.StationName)
	if err != nil {
		serv.Warnf("useSchemaDirect: Schema " + asr.Name + " at station " + asr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	exist, _, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("useSchemaDirect: Schema " + asr.Name + " at station " + asr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	if !exist {
		errMsg := "Station " + stationName.external + " does not exist"
		serv.Warnf("useSchemaDirect: " + errMsg)
		respondWithErr(s, reply, errors.New("memphis: "+errMsg))
		return
	}

	var schemaDetails models.SchemaDetails
	schemaName := strings.ToLower(asr.Name)
	exist, schema, err := IsSchemaExist(schemaName)
	if err != nil {
		serv.Errorf("useSchemaDirect: Schema " + asr.Name + " at station " + asr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}
	if !exist {
		errMsg := "Schema " + schemaName + " does not exist" + err.Error()
		serv.Warnf("useSchemaDirect: " + errMsg)
		respondWithErr(s, reply, errors.New(errMsg))
		return
	}

	schemaVersion, err := getActiveVersionBySchemaId(schema.ID)
	if err != nil {
		serv.Errorf("useSchemaDirect: Schema " + asr.Name + " at station " + asr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}
	schemaDetails = models.SchemaDetails{SchemaName: schemaName, VersionNumber: schemaVersion.VersionNumber}

	_, err = stationsCollection.UpdateOne(context.TODO(), bson.M{"name": stationName.Ext(), "is_deleted": false}, bson.M{"$set": bson.M{"schema": schemaDetails}})
	if err != nil {
		serv.Errorf("useSchemaDirect: Schema " + asr.Name + " at station " + asr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	username := c.getClientInfo(true).Name
	message := "Schema " + schemaName + " has been attached to station " + stationName.Ext() + " by user " + username
	serv.Noticef(message)

	var auditLogs []interface{}
	newAuditLog := models.AuditLog{
		ID:            primitive.NewObjectID(),
		StationName:   stationName.Intern(),
		Message:       message,
		CreatedByUser: username,
		CreationDate:  time.Now(),
		UserType:      "sdk",
	}
	auditLogs = append(auditLogs, newAuditLog)
	err = CreateAuditLogs(auditLogs)
	if err != nil {
		serv.Errorf("useSchemaDirect : Schema " + asr.Name + " at station " + asr.StationName + " - create audit logs: " + err.Error())
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		analytics.SendEvent("sdk", "user-attach-schema-to-station")
	}

	updateContent, err := generateSchemaUpdateInit(schema)
	if err != nil {
		serv.Errorf("useSchemaDirect: Schema " + asr.Name + " at station " + asr.StationName + ": " + err.Error())
		return
	}

	update := models.ProducerSchemaUpdate{
		UpdateType: models.SchemaUpdateTypeInit,
		Init:       *updateContent,
	}

	serv.updateStationProducersOfSchemaChange(stationName, update)
	respondWithErr(s, reply, nil)
}

func removeSchemaFromStation(s *Server, sn StationName, updateDB bool) error {
	exist, _, err := IsStationExist(sn)
	if err != nil {
		return err
	}
	if !exist {
		return errors.New("Station " + sn.external + " does not exist")
	}

	if updateDB {
		_, err = stationsCollection.UpdateOne(context.TODO(),
			bson.M{
				"name": sn.Ext(),
				"$or": []interface{}{
					bson.M{"is_deleted": false},
					bson.M{"is_deleted": bson.M{"$exists": false}},
				},
			},
			bson.M{"$set": bson.M{"schema": bson.M{}}},
		)
		if err != nil {
			return err
		}
	}

	update := models.ProducerSchemaUpdate{
		UpdateType: models.SchemaUpdateTypeDrop,
	}

	s.updateStationProducersOfSchemaChange(sn, update)
	return nil
}

func (s *Server) removeSchemaFromStationDirect(c *client, reply string, msg []byte) {
	var dsr detachSchemaRequest
	if err := json.Unmarshal(msg, &dsr); err != nil {
		s.Errorf("removeSchemaFromStationDirect: failed removing schema at station " + dsr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}
	stationName, err := StationNameFromStr(dsr.StationName)
	if err != nil {
		serv.Warnf("removeSchemaFromStationDirect: At station " + dsr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}

	err = removeSchemaFromStation(serv, stationName, true)
	if err != nil {
		serv.Errorf("removeSchemaFromStationDirect: At station " + dsr.StationName + ": " + err.Error())
		respondWithErr(s, reply, err)
		return
	}
	respondWithErr(s, reply, nil)
}

func (sh StationsHandler) RemoveSchemaFromStation(c *gin.Context) {
	if err := DenyForSandboxEnv(c); err != nil {
		return
	}

	var body models.RemoveSchemaFromStation
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	stationName, err := StationNameFromStr(body.StationName)
	if err != nil {
		serv.Warnf("RemoveSchemaFromStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
		return
	}
	exist, station, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("RemoveSchemaFromStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	if !exist {
		errMsg := "Station " + body.StationName + " does not exist"
		serv.Warnf("RemoveSchemaFromStation: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}

	err = removeSchemaFromStation(sh.S, stationName, true)
	if err != nil {
		serv.Errorf("RemoveSchemaFromStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	user, err := getUserDetailsFromMiddleware(c)
	if err != nil {
		serv.Errorf("RemoveSchemaFromStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(401, gin.H{"message": "Unauthorized"})
	}
	message := "Schema " + station.Schema.SchemaName + " has been deleted from station " + stationName.Ext() + " by user " + user.Username
	serv.Noticef(message)
	var auditLogs []interface{}
	newAuditLog := models.AuditLog{
		ID:            primitive.NewObjectID(),
		StationName:   stationName.Intern(),
		Message:       message,
		CreatedByUser: user.Username,
		CreationDate:  time.Now(),
		UserType:      user.UserType,
	}
	auditLogs = append(auditLogs, newAuditLog)
	err = CreateAuditLogs(auditLogs)
	if err != nil {
		serv.Errorf("RemoveSchemaFromStation: At station" + body.StationName + " - create audit logs error: " + err.Error())
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		analytics.SendEvent(user.Username, "user-remove-schema-from-station")
	}

	c.IndentedJSON(200, gin.H{})
}

func (sh StationsHandler) GetUpdatesForSchemaByStation(c *gin.Context) {
	var body models.GetUpdatesForSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	stationName, err := StationNameFromStr(body.StationName)
	if err != nil {
		serv.Warnf("GetUpdatesForSchemaByStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
		return
	}

	exist, station, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("GetUpdatesForSchemaByStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	if !exist {
		errMsg := "Station " + body.StationName + " does not exist"
		serv.Warnf("GetUpdatesForSchemaByStation: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}

	var schema models.Schema
	err = schemasCollection.FindOne(context.TODO(), bson.M{"name": station.Schema.SchemaName}).Decode(&schema)
	if err != nil {
		serv.Errorf("GetUpdatesForSchemaByStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}

	schemasHandler := SchemasHandler{S: sh.S}
	extedndedSchemaDetails, err := schemasHandler.getExtendedSchemaDetailsUpdateAvailable(station.Schema.VersionNumber, schema)

	if err != nil {
		serv.Errorf("GetUpdatesForSchemaByStation: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": err.Error()})
		return
	}

	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-apply-schema-updates-on-station")
	}

	c.IndentedJSON(200, extedndedSchemaDetails)
}

func (sh StationsHandler) TierdStorageClicked(c *gin.Context) {
	shouldSendAnalytics, _ := shouldSendAnalytics()
	if shouldSendAnalytics {
		user, _ := getUserDetailsFromMiddleware(c)
		analytics.SendEvent(user.Username, "user-pushed-tierd-storage-button")
	}

	c.IndentedJSON(200, gin.H{})
}

func (sh StationsHandler) UpdateDlsConfig(c *gin.Context) {
	var body models.UpdateDlsConfigSchema
	ok := utils.Validate(c, &body, false, nil)
	if !ok {
		return
	}

	stationName, err := StationNameFromStr(body.StationName)
	if err != nil {
		serv.Warnf("DlsConfiguration: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": err.Error()})
		return
	}

	exist, station, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("DlsConfiguration: At station" + body.StationName + ": " + err.Error())
		c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
		return
	}
	if !exist {
		errMsg := "Station " + body.StationName + " does not exist"
		serv.Warnf("DlsConfiguration: " + errMsg)
		c.AbortWithStatusJSON(configuration.SHOWABLE_ERROR_STATUS_CODE, gin.H{"message": errMsg})
		return
	}

	if station.DlsConfiguration.Poison != body.Poison || station.DlsConfiguration.Schemaverse != body.Schemaverse {
		dlsConfigurationNew := models.DlsConfiguration{
			Poison:      body.Poison,
			Schemaverse: body.Schemaverse,
		}
		filter := bson.M{
			"name": body.StationName,
			"$or": []interface{}{
				bson.M{"is_deleted": false},
				bson.M{"is_deleted": bson.M{"$exists": false}},
			}}

		update := bson.M{
			"$set": bson.M{
				"dls_configuration": dlsConfigurationNew,
			},
		}
		opts := options.Update().SetUpsert(true)

		_, err := stationsCollection.UpdateOne(context.TODO(), filter, update, opts)
		if err != nil {
			serv.Errorf("DlsConfiguration: At station" + body.StationName + ": " + err.Error())
			c.AbortWithStatusJSON(500, gin.H{"message": "Server error"})
			return
		}
	}
	c.IndentedJSON(200, gin.H{"poison": body.Poison, "schemaverse": body.Schemaverse})
}

func (s *Server) LaunchDlsForOldStations() error {
	var stations []models.Station
	cursor, err := stationsCollection.Find(context.TODO(), bson.M{
		"$or": []interface{}{
			bson.M{"is_deleted": false},
			bson.M{"is_deleted": bson.M{"$exists": false}},
		},
	})
	if err != nil {
		return err
	}

	if err = cursor.All(context.TODO(), &stations); err != nil {
		return err
	}
	for _, station := range stations {
		sn, err := StationNameFromStr(station.Name)
		if err != nil {
			return err
		}
		streamName := fmt.Sprintf(dlsStreamName, sn.Intern())

		_, err = s.memphisStreamInfo(streamName)
		if err != nil {
			if IsNatsErr(err, JSStreamNotFoundErr) {
				dlsConfigurationNew := models.DlsConfiguration{
					Poison:      true,
					Schemaverse: true,
				}
				filter := bson.M{
					"name": station.Name,
					"$or": []interface{}{
						bson.M{"is_deleted": false},
						bson.M{"is_deleted": bson.M{"$exists": false}},
					}}

				update := bson.M{
					"$set": bson.M{
						"dls_configuration": dlsConfigurationNew,
					},
				}
				opts := options.Update().SetUpsert(true)

				_, err := stationsCollection.UpdateOne(context.TODO(), filter, update, opts)
				if err != nil {
					return err
				}
				err = s.CreateDlsStream(sn, station)
				if err != nil {
					serv.Errorf("LaunchDlsForOldStations: CreateDlsStream: At station " + station.Name + ": " + err.Error())
					return err
				}
			} else {
				return err
			}
		}
	}
	return nil
}
