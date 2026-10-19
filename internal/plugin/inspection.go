package plugin

import (
	"encoding/json"
	"net/http"
	"path"
	"slices"
	"time"
)

type inspection struct {
	started       time.Time
	credentialIDs []string
}

type inspectionItem struct {
	credential
	Records any `json:"records"`
	// Degraded is null when no record from this inspection reaches a verdict, e.g. its requests failed.
	Degraded *bool `json:"degraded"`
}

var inspectionRuns = map[string]func([]byte) managementResponse{
	"candy": candyRunResponse, "fingerprint": fingerprintRunResponse,
}

// The latest inspection of each test, kept until the plugin stops.
var inspections = map[string]inspection{}

// inspectionRunResponse starts the test named in body on the enabled credentials whose email or name
// matches an include pattern (any credential when there are none) and no exclude pattern. The rest of
// body is that test's run request.
func inspectionRunResponse(body []byte) managementResponse {
	var req struct {
		Test    string   `json:"test"`
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
	}
	if json.Unmarshal(body, &req) != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误")
	}
	run := inspectionRuns[req.Test]
	if run == nil {
		return jsonError(http.StatusBadRequest, "未知测试类型")
	}
	auths, err := selectedCredentials(nil, true)
	if err != nil {
		return jsonError(http.StatusBadGateway, err.Error())
	}
	ids := []string{}
	for _, auth := range auths {
		if (len(req.Include) == 0 || matchesAny(auth, req.Include)) && !matchesAny(auth, req.Exclude) {
			ids = append(ids, auth.ID)
		}
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	fields["credential_ids"], _ = json.Marshal(ids)
	delete(fields, "all")
	body, _ = json.Marshal(fields)
	started := time.Now()
	response := run(body)
	if response.StatusCode == http.StatusOK {
		mu.Lock()
		inspections[req.Test] = inspection{started, ids}
		mu.Unlock()
	}
	return response
}

func matchesAny(auth credential, patterns []string) bool {
	for _, pattern := range patterns {
		for _, value := range []string{auth.Email, auth.Name} {
			if matched, _ := path.Match(pattern, value); matched {
				return true
			}
		}
	}
	return false
}

// inspectionResponse reports whether the latest inspection of test is still running, and the records each
// of its credentials got from it. A credential is degraded when any of them shows so: a wrong candy
// answer, or a fingerprint detection that attributes the answers to another model.
func inspectionResponse(test string) managementResponse {
	auths, err := credentials()
	if err != nil {
		return jsonError(http.StatusBadGateway, err.Error())
	}
	mu.Lock()
	defer mu.Unlock()
	latest, ok := inspections[test]
	if !ok {
		return jsonError(http.StatusNotFound, "还没有开始过这项巡检")
	}
	busy, items := false, []inspectionItem{}
	for _, auth := range auths {
		if !slices.Contains(latest.credentialIDs, auth.ID) {
			continue
		}
		busy = busy || running[auth.ID] != nil && running[auth.ID].Test == test
		item := inspectionItem{credential: auth}
		switch test {
		case "candy":
			item.Records, item.Degraded = inspected(records.Candies[auth.ID], latest.started, func(r candyRecord) (bool, bool) {
				return r.Status == "correct" || r.Status == "wrong", r.Status == "wrong"
			})
		case "fingerprint":
			found, degraded := inspected(records.Fingerprints[auth.ID], latest.started, func(r fingerprintRecord) (bool, bool) {
				if r.Attribution == nil || (r.Status != "completed" && r.Status != "partial") {
					return false, false
				}
				return true, fingerprintModelID(r.Model) != fingerprintModelID(r.Attribution.Prediction)
			})
			item.Records, item.Degraded = summaries(found), degraded
		}
		items = append(items, item)
	}
	return jsonResponse(http.StatusOK, map[string]any{"running": busy, "credentials": items})
}

// inspected returns the records of history made since started, and whether any of them shows a degraded
// model, or nil when none reaches a verdict. judge returns a record's verdict.
func inspected[T interface{ base() record }](history []T, started time.Time, judge func(T) (concluded, degraded bool)) ([]T, *bool) {
	found, anyConcluded, anyDegraded := []T{}, false, false
	for _, r := range history {
		if r.base().Time.Before(started) {
			continue
		}
		concluded, degraded := judge(r)
		found = append(found, r)
		anyConcluded = anyConcluded || concluded
		anyDegraded = anyDegraded || concluded && degraded
	}
	if !anyConcluded {
		return found, nil
	}
	return found, &anyDegraded
}
