SELECT snapshot_at AT TIME ZONE 'Asia/Irkutsk' AS "Снимок",completed_at AT TIME ZONE 'Asia/Irkutsk' AS "Обновлено",
 calculation_version AS "Версия расчёта",incomplete_count AS "Неполных заявок",
 CASE WHEN snapshot_at IS NULL THEN 'ВНИМАНИЕ: снимок ещё не рассчитан'
 WHEN now()-snapshot_at>interval '5 minutes' THEN 'ВНИМАНИЕ: данные старше пяти минут'
 WHEN last_error IS NOT NULL AND last_error<>'' THEN 'ВНИМАНИЕ: последнее обновление завершилось ошибкой' ELSE 'Данные актуальны' END AS "Состояние",
 last_attempt_at AT TIME ZONE 'Asia/Irkutsk' AS "Последняя попытка",last_error AS "Ошибка"
FROM analytics.refresh_state WHERE id=1
