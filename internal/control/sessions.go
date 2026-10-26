package control

import "time"

type Session struct {
	ClientIP      string `json:"client_ip"`
	UserAgent     string `json:"user_agent"`
	ClientName    string `json:"client_name"`
	ClientVersion string `json:"client_version"`
	ID            string `json:"id"`
	RouteID       string `json:"route_id"`
	NodeID        string `json:"node_id"`
	StartedAt     int64  `json:"started_at"`
	EndedAt       *int64 `json:"ended_at"`
	BytesSent     int64  `json:"bytes_sent"`
	Status        string `json:"status"`
}

func (s *Store) Sessions(limit int) ([]Session, error) {
	const startedAt = "CAST(json_extract(payload,'$.started_at') AS BIGINT)"
	rows, e := s.DB.Query(`SELECT * FROM (SELECT * FROM (SELECT id,route_id,'local' AS node_id,started_at,ended_at,bytes_sent,status,client_ip,user_agent,client_name,client_version FROM stream_sessions ORDER BY started_at DESC LIMIT ?) local_part UNION ALL SELECT * FROM (SELECT id,json_extract(payload,'$.route_id') AS route_id,node_id,`+startedAt+` AS started_at,CAST(json_extract(payload,'$.ended_at') AS BIGINT) AS ended_at,CAST(json_extract(payload,'$.bytes_sent') AS BIGINT) AS bytes_sent,CASE WHEN json_extract(payload,'$.status')='active' AND updated_at<? THEN 'unknown' ELSE json_extract(payload,'$.status') END AS status,COALESCE(json_extract(payload,'$.client_ip'),'') AS client_ip,COALESCE(json_extract(payload,'$.user_agent'),'') AS user_agent,COALESCE(json_extract(payload,'$.client_name'),'') AS client_name,COALESCE(json_extract(payload,'$.client_version'),'') AS client_version FROM remote_sessions ORDER BY `+startedAt+` DESC LIMIT ?) remote_part) merged ORDER BY started_at DESC LIMIT ?`, limit, time.Now().Unix()-60, limit, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var x Session
		if e = rows.Scan(&x.ID, &x.RouteID, &x.NodeID, &x.StartedAt, &x.EndedAt, &x.BytesSent, &x.Status, &x.ClientIP, &x.UserAgent, &x.ClientName, &x.ClientVersion); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
