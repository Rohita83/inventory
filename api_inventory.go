// Copyright 2016 Mender Software AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package main

import (
	"github.com/ant0ine/go-json-rest/rest"
	"github.com/asaskevich/govalidator"
	"github.com/mendersoftware/inventory/config"
	"github.com/mendersoftware/inventory/log"
	"github.com/mendersoftware/inventory/requestlog"
	"github.com/mendersoftware/inventory/utils"
	"github.com/mendersoftware/inventory/utils/identity"
	"github.com/pkg/errors"
	"net/http"
	"strconv"
	"strings"
)

const (
	uriDevices     = "/api/0.1.0/devices"
	uriDevice      = "/api/0.1.0/devices/:id"
	uriDeviceGroup = "/api/0.1.0/devices/:id/group/:name"
	uriAttributes  = "/api/0.1.0/attributes"

	LogHttpCode = "http_code"
)

const (
	queryParamSort           = "sort"
	queryParamHasGroup       = "has_group"
	queryParamValueSeparator = ":"
	sortOrderAsc             = "asc"
	sortOrderDesc            = "desc"
	sortAttributeNameIdx     = 0
	sortOrderIdx             = 1
	filterEqOperatorIdx      = 0
)

// model of device's group name response at /devices/:id/group endpoint
type InventoryApiGroup struct {
	Group string `json:"group"`
}

type InventoryFactory func(c config.Reader, l *log.Logger) (InventoryApp, error)

type InventoryHandlers struct {
	createInventory InventoryFactory
}

// return an ApiHandler for device admission app
func NewInventoryApiHandlers(invF InventoryFactory) ApiHandler {
	return &InventoryHandlers{
		invF,
	}
}

func (i *InventoryHandlers) GetApp() (rest.App, error) {
	routes := []*rest.Route{
		rest.Get(uriDevices, i.GetDevicesHandler),
		rest.Post(uriDevices, i.AddDeviceHandler),
		rest.Delete(uriDeviceGroup, i.DeleteDeviceGroupHandler),
		rest.Patch(uriAttributes, i.PatchDeviceAttributesHandler),
	}

	routes = append(routes)

	app, err := rest.MakeRouter(
		// augment routes with OPTIONS handler
		AutogenOptionsRoutes(routes, AllowHeaderOptionsGenerator)...,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create router")
	}

	return app, nil

}

// `sort` paramater value is an attribute name with optional direction (desc or asc)
// separated by colon (:)
//
// eg. `sort=attr_name1` or `sort=attr_name1:asd`
func parseSortParam(r *rest.Request) (*Sort, error) {
	sortStr, err := utils.ParseQueryParmStr(r, queryParamSort, false, nil)
	if err != nil {
		return nil, err
	}
	if sortStr == "" {
		return nil, nil
	}
	sortValArray := strings.Split(sortStr, queryParamValueSeparator)
	sort := Sort{AttrName: sortValArray[sortAttributeNameIdx]}
	if len(sortValArray) == 2 {
		sortOrder := sortValArray[sortOrderIdx]
		if sortOrder != sortOrderAsc && sortOrder != sortOrderDesc {
			return nil, errors.New("invalid sort order")
		}
		sort.Ascending = sortOrder == sortOrderAsc
	}
	return &sort, nil
}

// Filter paramaters name are attributes name. Value can be prefixed
// with equality operator code (`eq` for =), separated from value by colon (:).
// Equality operator default value is `eq`
//
// eg. `attr_name1=value1` or `attr_name1=eq:value1`
func parseFilterParams(r *rest.Request) ([]Filter, error) {
	knownParams := []string{utils.PageName, utils.PerPageName, queryParamSort, queryParamHasGroup}
	filters := make([]Filter, 0)
	var filter Filter
	for name, _ := range r.URL.Query() {
		if utils.ContainsString(name, knownParams) {
			continue
		}
		valueStr, err := utils.ParseQueryParmStr(r, name, false, nil)
		if err != nil {
			return nil, err
		}
		valueStrArray := strings.Split(valueStr, queryParamValueSeparator)
		filter = Filter{AttrName: name}
		valueIdx := 0
		if len(valueStrArray) == 2 {
			valueIdx = 1
			switch valueStrArray[filterEqOperatorIdx] {
			case "eq":
				filter.Operator = Eq
			default:
				return nil, errors.New("invalid filter operator")
			}
		} else {
			filter.Operator = Eq
		}
		filter.Value = valueStrArray[valueIdx]
		floatValue, err := strconv.ParseFloat(filter.Value, 64)
		if err == nil {
			filter.ValueFloat = &floatValue
		}

		filters = append(filters, filter)
	}
	return filters, nil
}

func (i *InventoryHandlers) GetDevicesHandler(w rest.ResponseWriter, r *rest.Request) {
	l := requestlog.GetRequestLogger(r.Env)

	page, perPage, err := utils.ParsePagination(r)
	if err != nil {
		restErrWithLog(w, l, err, http.StatusBadRequest)
		return
	}

	hasGroup, err := utils.ParseQueryParmBool(r, queryParamHasGroup, false, nil)
	if err != nil {
		restErrWithLog(w, l, err, http.StatusBadRequest)
		return
	}

	sort, err := parseSortParam(r)
	if err != nil {
		restErrWithLog(w, l, err, http.StatusBadRequest)
		return
	}

	filters, err := parseFilterParams(r)
	if err != nil {
		restErrWithLog(w, l, err, http.StatusBadRequest)
		return
	}

	inv, err := i.createInventory(config.Config, l)
	if err != nil {
		restErrWithLogInternal(w, l, err)
		return
	}

	//get one extra device to see if there's a 'next' page
	devs, err := inv.ListDevices(int((page-1)*perPage), int(perPage+1), filters, sort, hasGroup)
	if err != nil {
		restErrWithLogInternal(w, l, err)
		return
	}

	len := len(devs)
	hasNext := false
	if uint64(len) > perPage {
		hasNext = true
		len = int(perPage)
	}

	links := utils.MakePageLinkHdrs(r, page, perPage, hasNext)
	for _, l := range links {
		w.Header().Add("Link", l)
	}
	w.WriteJson(devs[:len])
}

func (i *InventoryHandlers) AddDeviceHandler(w rest.ResponseWriter, r *rest.Request) {
	l := requestlog.GetRequestLogger(r.Env)

	dev, err := parseDevice(r)
	if err != nil {
		restErrWithLog(w, l, err, http.StatusBadRequest)
		return
	}

	inv, err := i.createInventory(config.Config, l)
	if err != nil {
		restErrWithLogInternal(w, l, err)
		return
	}

	err = inv.AddDevice(dev)
	if err != nil {
		if cause := errors.Cause(err); cause != nil && cause == ErrDuplicatedDeviceId {
			restErrWithLogMsg(w, l, err, http.StatusConflict, "device with specified ID already exists")
			return
		}
		restErrWithLogInternal(w, l, err)
		return
	}

	devurl := utils.BuildURL(r, uriDevice, map[string]string{
		":id": dev.ID.String(),
	})
	w.Header().Add("Location", devurl.String())
	w.WriteHeader(http.StatusCreated)
}

func (i *InventoryHandlers) PatchDeviceAttributesHandler(w rest.ResponseWriter, r *rest.Request) {
	l := requestlog.GetRequestLogger(r.Env)

	//get device ID from JWT token
	idata, err := identity.ExtractIdentityFromHeaders(r.Header)
	if err != nil {
		restErrWithLogMsg(w, l, err, http.StatusUnauthorized, "unauthorized")
		return
	}

	//extract attributes from body
	attrs, err := parseAttributes(r)
	if err != nil {
		restErrWithLog(w, l, err, http.StatusBadRequest)
		return
	}

	//upsert the attributes
	inv, err := i.createInventory(config.Config, l)
	if err != nil {
		restErrWithLogInternal(w, l, err)
		return
	}

	err = inv.UpsertAttributes(DeviceID(idata.Subject), attrs)
	if err != nil {
		restErrWithLogInternal(w, l, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (i *InventoryHandlers) DeleteDeviceGroupHandler(w rest.ResponseWriter, r *rest.Request) {
	l := requestlog.GetRequestLogger(r.Env)

	deviceID := r.PathParam("id")
	groupName := r.PathParam("name")

	inv, err := i.createInventory(config.Config, l)
	if err != nil {
		restErrWithLogInternal(w, l, err)
		return
	}

	err = inv.UnsetDeviceGroup(DeviceID(deviceID), GroupName(groupName))
	if err != nil {
		cause := errors.Cause(err)
		if cause != nil {
			if cause.Error() == ErrDevNotFound.Error() {
				restErrWithLog(w, l, err, http.StatusNotFound)
				return
			} else if cause.Error() == ErrDevNotInGivenGroup.Error() {
				restErrWithLog(w, l, err, http.StatusBadRequest)
				return
			}
		}
		restErrWithLogInternal(w, l, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func parseDevice(r *rest.Request) (*Device, error) {
	dev := Device{}

	//decode body
	err := r.DecodeJsonPayload(&dev)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode request body")
	}

	if err := dev.Validate(); err != nil {
		return nil, err
	}

	return &dev, nil
}

func parseAttributes(r *rest.Request) (DeviceAttributes, error) {
	var attrs DeviceAttributes

	err := r.DecodeJsonPayload(&attrs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode request body")
	}

	for _, a := range attrs {
		if _, err = govalidator.ValidateStruct(a); err != nil {
			return nil, err
		}
	}

	return attrs, nil
}

// return selected http code + error message directly taken from error
// log error
func restErrWithLog(w rest.ResponseWriter, l *log.Logger, e error, code int) {
	restErrWithLogMsg(w, l, e, code, e.Error())
}

// return http 500, with an "internal error" message
// log full error
func restErrWithLogInternal(w rest.ResponseWriter, l *log.Logger, e error) {
	msg := "internal error"
	e = errors.Wrap(e, msg)
	restErrWithLogMsg(w, l, e, http.StatusInternalServerError, msg)
}

// return an error code with an overriden message (to avoid exposing the details)
// log full error
func restErrWithLogMsg(w rest.ResponseWriter, l *log.Logger, e error, code int, msg string) {
	rest.Error(w, msg, code)
	l.F(log.Ctx{LogHttpCode: code}).Error(errors.Wrap(e, msg).Error())
}
