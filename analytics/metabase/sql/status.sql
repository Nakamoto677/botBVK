SELECT
 CASE WHEN snapshot_at IS NULL THEN 'Нет снимка — показатели пока недоступны'
 WHEN last_error IS NOT NULL AND last_error<>'' THEN 'Ошибка обновления — показан предыдущий снимок'
 WHEN now()-snapshot_at>interval '5 minutes' THEN 'Данные устарели — проверьте обновление'
 ELSE 'Обновляется каждую минуту' END AS "Данные",
 to_char(snapshot_at AT TIME ZONE 'Asia/Irkutsk','DD.MM.YYYY HH24:MI') AS "Снимок · Иркутск",
 CASE WHEN incomplete_count>0 THEN 'Неполная история: '||incomplete_count||' заявок' ELSE 'Без выявленных пробелов' END AS "История"
 FROM analytics.refresh_state WHERE id=1
