package dlna

import (
	"encoding/xml"
	"net/http"
)

type serviceDescription struct {
	XMLName   xml.Name          `xml:"scpd"`
	Namespace string            `xml:"xmlns,attr"`
	Version   serviceVersion    `xml:"specVersion"`
	Actions   []serviceAction   `xml:"actionList>action"`
	Variables []serviceVariable `xml:"serviceStateTable>stateVariable"`
}
type serviceVersion struct {
	Major int `xml:"major"`
	Minor int `xml:"minor"`
}
type serviceAction struct {
	Name      string            `xml:"name"`
	Arguments []serviceArgument `xml:"argumentList>argument,omitempty"`
}
type serviceArgument struct {
	Name      string `xml:"name"`
	Direction string `xml:"direction"`
	Variable  string `xml:"relatedStateVariable"`
}
type serviceVariable struct {
	Events string `xml:"sendEvents,attr"`
	Name   string `xml:"name"`
	Type   string `xml:"dataType"`
}

func arg(name, direction, variable string) serviceArgument {
	return serviceArgument{name, direction, variable}
}
func variable(name, typ string, event bool) serviceVariable {
	events := "no"
	if event {
		events = "yes"
	}
	return serviceVariable{events, name, typ}
}
func contentService() serviceDescription {
	in, out := "in", "out"
	return serviceDescription{Namespace: "urn:schemas-upnp-org:service-1-0", Version: serviceVersion{1, 0}, Actions: []serviceAction{
		{"Browse", []serviceArgument{arg("ObjectID", in, "A_ARG_TYPE_ObjectID"), arg("BrowseFlag", in, "A_ARG_TYPE_BrowseFlag"), arg("Filter", in, "A_ARG_TYPE_Filter"), arg("StartingIndex", in, "A_ARG_TYPE_Index"), arg("RequestedCount", in, "A_ARG_TYPE_Count"), arg("SortCriteria", in, "A_ARG_TYPE_SortCriteria"), arg("Result", out, "A_ARG_TYPE_Result"), arg("NumberReturned", out, "A_ARG_TYPE_Count"), arg("TotalMatches", out, "A_ARG_TYPE_Count"), arg("UpdateID", out, "A_ARG_TYPE_UpdateID")}},
		{"Search", []serviceArgument{arg("ContainerID", in, "A_ARG_TYPE_ObjectID"), arg("SearchCriteria", in, "A_ARG_TYPE_SearchCriteria"), arg("Filter", in, "A_ARG_TYPE_Filter"), arg("StartingIndex", in, "A_ARG_TYPE_Index"), arg("RequestedCount", in, "A_ARG_TYPE_Count"), arg("SortCriteria", in, "A_ARG_TYPE_SortCriteria"), arg("Result", out, "A_ARG_TYPE_Result"), arg("NumberReturned", out, "A_ARG_TYPE_Count"), arg("TotalMatches", out, "A_ARG_TYPE_Count"), arg("UpdateID", out, "A_ARG_TYPE_UpdateID")}},
		{"GetSearchCapabilities", []serviceArgument{arg("SearchCaps", out, "SearchCapabilities")}},
		{"GetSortCapabilities", []serviceArgument{arg("SortCaps", out, "SortCapabilities")}},
		{"GetSystemUpdateID", []serviceArgument{arg("Id", out, "SystemUpdateID")}},
	}, Variables: []serviceVariable{
		variable("A_ARG_TYPE_ObjectID", "string", false), variable("A_ARG_TYPE_BrowseFlag", "string", false), variable("A_ARG_TYPE_Filter", "string", false), variable("A_ARG_TYPE_Index", "ui4", false), variable("A_ARG_TYPE_Count", "ui4", false), variable("A_ARG_TYPE_SortCriteria", "string", false), variable("A_ARG_TYPE_SearchCriteria", "string", false), variable("A_ARG_TYPE_Result", "string", false), variable("A_ARG_TYPE_UpdateID", "ui4", false), variable("SearchCapabilities", "string", false), variable("SortCapabilities", "string", false), variable("SystemUpdateID", "ui4", true),
	}}
}
func connectionService() serviceDescription {
	in, out := "in", "out"
	return serviceDescription{Namespace: "urn:schemas-upnp-org:service-1-0", Version: serviceVersion{1, 0}, Actions: []serviceAction{
		{"GetProtocolInfo", []serviceArgument{arg("Source", out, "SourceProtocolInfo"), arg("Sink", out, "SinkProtocolInfo")}},
		{"GetCurrentConnectionIDs", []serviceArgument{arg("ConnectionIDs", out, "CurrentConnectionIDs")}},
		{"GetCurrentConnectionInfo", []serviceArgument{arg("ConnectionID", in, "A_ARG_TYPE_ConnectionID"), arg("RcsID", out, "A_ARG_TYPE_RcsID"), arg("AVTransportID", out, "A_ARG_TYPE_AVTransportID"), arg("ProtocolInfo", out, "A_ARG_TYPE_ProtocolInfo"), arg("PeerConnectionManager", out, "A_ARG_TYPE_ConnectionManager"), arg("PeerConnectionID", out, "A_ARG_TYPE_ConnectionID"), arg("Direction", out, "A_ARG_TYPE_Direction"), arg("Status", out, "A_ARG_TYPE_ConnectionStatus")}},
	}, Variables: []serviceVariable{
		variable("SourceProtocolInfo", "string", true), variable("SinkProtocolInfo", "string", true), variable("CurrentConnectionIDs", "string", true), variable("A_ARG_TYPE_ConnectionID", "i4", false), variable("A_ARG_TYPE_RcsID", "i4", false), variable("A_ARG_TYPE_AVTransportID", "i4", false), variable("A_ARG_TYPE_ProtocolInfo", "string", false), variable("A_ARG_TYPE_ConnectionManager", "string", false), variable("A_ARG_TYPE_Direction", "string", false), variable("A_ARG_TYPE_ConnectionStatus", "string", false),
	}}
}
func writeService(w http.ResponseWriter, value serviceDescription) {
	data, err := xml.Marshal(value)
	if err != nil {
		http.Error(w, "service description unavailable", http.StatusInternalServerError)
		return
	}
	writeXML(w, string(data))
}
