-- Preserve subscriptions and accounting while detaching deleted assigned routes.
UPDATE objects SET payload=(payload::jsonb || '{"route_id":"","key_id":"","credentials":""}'::jsonb)::text WHERE kind='access_account' AND COALESCE(json_extract(payload,'$.route_id'),'')<>'' AND NOT EXISTS (SELECT 1 FROM objects route WHERE route.kind='route' AND route.id=json_extract(objects.payload,'$.route_id'));
