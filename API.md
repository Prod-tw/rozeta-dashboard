# External API

## 查詢目前進行中的議程

查詢指定 room 目前可確認的議程。此 endpoint 不需要 session 或 API token。

```http
GET /api/v1/rooms/{room_name}/in-progress
```

controller 會查詢 Rozeta 完整分頁的 `status=in_progress` 結果：

- 沒有進行中的議程時回傳 `null`。
- 只有一個進行中的議程時回傳該議程。
- 有多個進行中的議程時，只在 controller 的 desired meeting 也正在進行時回傳它。
- 有多個進行中的議程但 desired meeting 不在其中時回傳 `null`，不依 Rozeta 回傳順序猜測。

### 成功回應

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
	"name": "開源社群治理",
	"opass_id": "opass-session-123"
}
```

沒有可確認的議程時：

```json
null
```

`name` 是 Rozeta meeting 的 `title`；`opass_id` 是 `session.csv` 的 `Session ID`，不是 Rozeta meeting ID。只有通過啟動時 OPASS、session CSV 與 Rozeta 交集驗證的議程才會被回傳。

### 錯誤回應

| HTTP  | 情況                               |
| ----- | ---------------------------------- |
| `404` | room 不存在                        |
| `502` | 無法從 Rozeta 取得完整的進行中議程 |

## 切換至下一場並啟動 reconciliation

讓外部系統透過單一明確確認，要求指定 room 將排程中的下一個 meeting 設為 desired meeting，並在需要時啟動 reconciliation。

```http
POST /api/v1/rooms/{room_name}/actions/advance-and-start
Authorization: Bearer <external-api-token>
```

此 API 不接受啟停布林狀態。POST 本身代表呼叫端已確認 Start，以及 completed meeting 可能觸發的破壞性自動 Resume；呼叫端必須先向操作者警告 Resume 會永久刪除已完成的逐字稿與翻譯。

### 行為

1. 以目前 persisted desired meeting 為基準，依排程選擇下一個 meeting。
2. 驗證 room 未在 `stopping`，並以目前 process epoch 與 reconciliation run 防止舊操作影響新 run。
3. Preflight 讀取下一個 meeting 的最新狀態，以及完整分頁的 `status=in_progress` active set。
4. Preflight 成功後，將下一個 meeting 與新 generation 原子寫入 state v3；新 generation 取得一次自動 Resume 額度。
5. 若 room 為 `suspended`，建立新 run 並依序進入 `starting`、`active`；若已是 `active`，立即 reconcile 新 generation。
6. Reconciliation 先讓新 desired meeting 達到 `in_progress`，確認後才 Pause 其他 active meetings，最終收斂至 active set 恰為 `{desired}`。

「下一場」只考慮具有 `scheduled_start` 的 meeting，並依 `scheduled_start`、title、ID 排序。未排程的 meeting 不使用 Rozeta API 回傳順序推測位置。Session schedule 只提供推薦與排序，不會自行選擇 desired meeting。

若下一場為 `completed`，controller 會在 dispatch 前原子記錄該 generation 與 completed `updated_at` 已消耗自動 Resume。即使 Resume timeout、結果不明或程序重啟，同一 generation 也不會再次自動 Resume。再次傳送相同 desired update 不會重新取得額度；必須使用獨立的破壞性 re-arm 操作建立新 generation。

`202 Accepted` 只表示 desired generation 與 lifecycle 操作已接受，不表示 Rozeta 已完成 Goto、Start、Resume 或舊 meeting cleanup。呼叫端應透過管理 snapshot 追蹤 lifecycle、active meeting IDs 與 condition。

### 成功回應

```http
HTTP/1.1 202 Accepted
Content-Type: application/json
```

```json
{
	"room_name": "R0",
	"meeting_id": "meeting-b",
	"generation": 8,
	"lifecycle": "starting",
	"status": "accepted"
}
```

### 錯誤回應

| HTTP  | `code`                        | 情況                                                                          |
| ----- | ----------------------------- | ----------------------------------------------------------------------------- |
| `401` | `authentication_required`     | API token 缺少或無效                                                          |
| `404` | `room_not_found`              | room 不存在                                                                   |
| `409` | `current_meeting_unset`       | room 尚未設定目前的 desired meeting                                           |
| `409` | `current_meeting_unscheduled` | 目前的 desired meeting 不在排程內                                             |
| `409` | `next_meeting_not_found`      | 目前的 desired meeting 已是排程中的最後一場                                   |
| `409` | `room_stopping`               | room 正在停止，無法接受新操作                                                 |
| `409` | `stale_controller_state`      | process epoch 或 reconciliation run 已過期                                    |
| `503` | `preflight_unavailable`       | 無法完整觀察下一場狀態或 active set；不接受 desired generation，也不啟動 room |
| `503` | `schedule_unavailable`        | 沒有可用的 meeting 排程                                                       |

錯誤回應使用以下格式：

```json
{
	"error": {
		"code": "next_meeting_not_found",
		"message": "the room is already at the final scheduled meeting"
	}
}
```

## Lifecycle 操作共通規則

- Start、Stop、Force-stop 都是需明確確認的操作；bulk 操作只處理確認流程凍結的 room 清單。
- Normal Stop 的 remote target 是空 active set。Preflight 會列出即將 Pause 的所有 `in_progress` meetings；active set 無法完整觀察時不接受 normal Stop。
- Stop 接受後會停用自動 Resume、拒絕新 Goto/Start/Resume/desired update，並反覆 Observe、Pause，直到 fresh observation 確認 active set 為空才回到 `suspended`。
- `stopping` 立即提供 Force-stop，30 秒後也會自動 force-stop。Force-stop 取消並忽略舊 run 的本機工作，但無法撤回 Rozeta 已接受的命令，因此結果為 `suspended / RemoteOutcomeUnknown`。

## 管理介面：議程偏差

管理員可保存每間教室的議程偏差。正數代表現場延遲，負數代表提前；此設定只影響 browser 端的時間警報計算，不會自動切換 meeting。

```http
PUT /api/rooms/{room_name}/schedule-offset
```

```json
{
	"minutes": 10,
	"epoch": "process-epoch",
	"expected_reconciliation_run": 4,
	"expected_generation": 8,
	"expected_revision": 31
}
```

偏差必須介於 `-120` 到 `120` 分鐘。警報開關、警報門檻、通知權限與通知狀態全部由 browser 保存，不會寫入 server state。舊版 state 檔案不提供 migration，必須重新建立 v3 state。

操作頁 `/` 也支援 client-only 測試時間模式。操作員可在頁面工具選單設定 `alert_test_at`，設定後會回到目前頁面並以指定時間為起點持續 1:1 流逝；恢復真實時間會移除該 query parameter。`/debug` 不使用這項功能。
