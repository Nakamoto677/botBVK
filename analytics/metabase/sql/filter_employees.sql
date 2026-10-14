SELECT id AS value,name AS label FROM analytics.employees WHERE role IN ('engineer','admin','dispatcher') ORDER BY name,id
