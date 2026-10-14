# Аналитика BVK: Windows, права и восстановление

Подготовлены установочные материалы; службы, домен, аккаунты и внешнее HTTPS в этой
разработке **не развёрнуты**. Команды ниже выполняет администратор на Windows-сервере.
Сначала используйте копию рабочей БД и сравните реальные контрольные заявки с
[правилами панели](METABASE.md). Нативный запуск PowerShell/NSSM, ACL,
Metabase, внешние порты и восстановление требуют отдельной проверки внедрения.

В изолированном PostgreSQL проверены `provision.sql` и `check-privileges.sql`:
расчётчик читает исходные таблицы и пишет витрину; Metabase читает витрину;
чтение источника и запись витрины читателем, запись исходных заявок расчётчиком
отклоняются PostgreSQL. Это проверка SQL-прав, а не работающей Windows-панели.

## Зафиксированные зависимости

Срез официальных релизов проверен 13 октября 2026:

| Компонент | Версия | Официальный источник |
|---|---|---|
| Metabase OSS JAR | `v0.64.1` | [релиз](https://github.com/metabase/metabase/releases/tag/v0.64.1), [JAR](https://downloads.metabase.com/v0.64.1.x/metabase.jar) |
| Eclipse Temurin JDK x64 | `21.0.12.1+1` | [релиз](https://github.com/adoptium/temurin21-binaries/releases/tag/jdk-21.0.12.1%2B1) |
| Caddy Windows amd64 | `v2.11.7` | [релиз](https://github.com/caddyserver/caddy/releases/tag/v2.11.7) |
| NSSM x64 | `2.24` | [поставка](https://nssm.cc/download), [настройки служб](https://nssm.cc/usage) |
| PostgreSQL/tools | используемая сервером версия; пилот — 17 | `pg_dump`/`pg_restore` совместимы с сервером |
| Сборка BVK | Go 1.26, текущая ревизия репозитория | `cmd/bvk-analytics` |

Metabase требует Java 21 или новее и запуск JAR с
`--add-opens java.base/java.nio=ALL-UNNAMED` ([инструкция](https://www.metabase.com/docs/latest/installation-and-operation/running-the-metabase-jar-file)).
Закрепление версии не заменяет регулярную проверку обновлений безопасности.
Обновляйте компоненты после копии, тестового восстановления и проверки дашбордов.

Скачайте конкретные версии из источников таблицы. Сверьте checksum дистрибутива
Temurin и Caddy с опубликованными файлами релиза; проверьте подпись установщика
Java, если используется MSI. Для JAR, NSSM и собственного бинарника запишите
SHA256 доверенной загрузки/сборки в реестр поставки. Если upstream не публикует
отдельную checksum JAR, записанный hash закрепляет проверенную поставку, но не
служит независимым доказательством происхождения. Сохраните исходные архивы,
checksum и номер ревизии вне каталога приложения.

Скопируйте `scripts/analytics/versions.json` и заполните все `sha256` через
`Get-FileHash -Algorithm SHA256`. Hash Java в этом файле относится к уже
распакованному `java.exe`; дополнительно обязательно проверьте исходный архив.
Установщик откажется работать с заглушками или несовпадающими файлами. Он не
скачивает зависимости и не заявляет о проверенной checksum заранее.

## Каталоги и разделение процессов

Рекомендуемый отдельный каталог `C:\BVKAnalytics`:

```text
bin\bvk-analytics.exe       bin\caddy.exe
java\bin\java.exe           metabase\metabase.jar
config.json                 Caddyfile
versions.json               run-service.ps1
secrets\analytics.json      secrets\metabase.json      secrets\caddy.json
data\logs\                  data\caddy\               data\caddy-config\
```

Соберите `GOOS=windows GOARCH=amd64 go build -trimpath -o bin/bvk-analytics.exe
./cmd/bvk-analytics` в среде сборки; на Windows задайте GOOS/GOARCH через `$env:`.
Скопируйте `Caddyfile`, `versions.json`, `run-service.ps1` из `scripts/analytics`.
После проверки происхождения скачанных скриптов снимите Windows Zone.Identifier
через `Unblock-File`, если RemoteSigned блокирует локальный запуск.
`config.json` — актуальная конфигурация бота с тем же календарём и порогами; в ней
нет токена и паролей. Аналитика использует настройки календаря и не запускает
бота, bootstrap, Telegram-проверки или миграции исходных таблиц.

Бот остаётся отдельной существующей службой `BVKSupport`. Не заменяйте её
DATABASE_URL аналитическим пользователем и не используйте её привилегированную
учётную запись для аналитики. Новый процесс непрерывно пересчитывает снимок раз
в минуту; `-once` делает один расчёт и выходит, `-migrate` создаёт/обновляет только
схему аналитики и выходит. Миграции выполняет владелец БД, служба — ограниченная
роль. Не передавайте `-migrate` в службу.

## PostgreSQL: три разных подключения

1. От имени владельца исходной БД: задайте `DATABASE_URL` и выполните
   `bvk-analytics.exe -config config.json -migrate`.
2. От администратора PostgreSQL выполните
   `psql -X -v ON_ERROR_STOP=1 -v source_db=bvk -f scripts/analytics/provision.sql`.
   Замените `bvk` реальным именем БД. Скрипт рассчитан на новые, свободные имена
   ролей; повторный запуск откажется переиспользовать неизвестные роли. Создание
   БД после транзакции может потребовать ручного продолжения при сбое.
3. Задайте три разные случайные пароля через интерактивный `\password` в psql:
   `bvk_analytics_writer`, `bvk_metabase_reader`, `bvk_metabase_app`. Пароли не
   вводите в shell-команду/историю или SQL-файл.

| Роль | Назначение и разрешения |
|---|---|
| `bvk_analytics_writer` | SELECT пяти `public.bvk_*` таблиц: tickets, messages, events, users, companies; SELECT/INSERT/UPDATE/DELETE в `analytics` |
| `bvk_metabase_reader` | SELECT только таблиц `analytics` в исходной БД |
| `bvk_metabase_app` | владелец отдельной БД `bvk_metabase`, запись настроек, аккаунтов и карточек Metabase |

Ни один runtime-пользователь аналитики не должен владеть исходной БД/таблицами,
иметь SUPERUSER/CREATEDB/CREATEROLE, членство в старых широких ролях или CREATE
в исходной схеме. PostgreSQL суммирует права, включая PUBLIC: GRANT скрипта не
отменяет ранее выданных PUBLIC прав. Запустите `check-privileges.sql` в исходной
БД; при старой установке PostgreSQL с PUBLIC CREATE на `public` отдельно
согласуйте `REVOKE CREATE ON SCHEMA public FROM PUBLIC` и права владельца бота.
Проверьте также SECURITY DEFINER функции и `pg_hba.conf`: эти роли должны
подключаться только к нужным БД с локального сервера; запретите остальные
подключения адресными HBA-правилами. Не публикуйте PostgreSQL в интернет.

`ALTER DEFAULT PRIVILEGES` относится к роли, выполняющей provision. Если миграции
выполняет другой владелец, повторите эти два GRANT `FOR ROLE имя_владельца`;
после каждой миграции повторно выдайте права на новые таблицы и выполните проверку.
Последовательности/TRUNCATE/CREATE runtime-процессу не требуются.

Отдельная прикладная БД обязательна: она хранит аккаунты, сохранённые запросы,
дашборды и зашифрованные реквизиты подключения; H2 здесь не используется
([документация](https://www.metabase.com/docs/latest/installation-and-operation/configuring-application-database)).

## Службы и секреты

Запустите повышенный PowerShell (Windows PowerShell 5.1 или совместимый):

```powershell
.\scripts\analytics\install-services.ps1 -InstallDir 'C:\BVKAnalytics' -NssmPath 'C:\Tools\nssm.exe'
```

Установщик проверяет наличие/hash файлов, результат каждой внешней команды и
создаёт три **остановленные** службы с виртуальными учётными записями:
`NT SERVICE\BVKAnalytics`, `NT SERVICE\BVKMetabase`, `NT SERVICE\BVKCaddy`.
Если конкретная Windows/NSSM не принимает виртуальную запись, настройте отдельную
локальную служебную запись без административных прав и заново назначьте ACL.
Не переключайте на LocalSystem. При частичном сбое уже созданные службы остаются
остановленными; проверьте ошибку и завершите/удалите эти новые службы вручную.

Установщик отключает наследование ACL каталога и secrets, оставляет полный доступ
Administrators/SYSTEM, даёт runtime чтение неизменяемых файлов, запись только в
свои логи и Caddy-хранилище. Файлы secrets читаются только соответствующей службой
и администраторами. Удалите старые явные ACL других пользователей, если каталог
не новый; `icacls` не удаляет автоматически все старые явные записи. NSSM и Java
также должны быть защищены от записи непривилегированными пользователями.

После создания защищённых файлов заполните их локально как администратор:

```json
// secrets/analytics.json — пример структуры, не готовые реквизиты
{"DATABASE_URL":"postgres://bvk_analytics_writer:URL_ENCODED_PASSWORD@127.0.0.1:5432/bvk?sslmode=disable"}
```

```json
// secrets/metabase.json
{"MB_DB_HOST":"127.0.0.1","MB_DB_PORT":"5432","MB_DB_DBNAME":"bvk_metabase","MB_DB_USER":"bvk_metabase_app","MB_DB_PASS":"REPLACE","MB_ENCRYPTION_SECRET_KEY":"REPLACE_WITH_RANDOM_BASE64","MB_SITE_URL":"https://support.example.org"}
```

```json
// secrets/caddy.json
{"ANALYTICS_DOMAIN":"support.example.org","ACME_EMAIL":"admin@example.org"}
```

JSON-файлы не содержат комментариев строк `//`. Локальный `sslmode=disable`
приемлем только для loopback; при отдельном PostgreSQL используйте TLS с проверкой
сертификата. Ключ Metabase — минимум 32 случайных байта в Base64; сохраните его
отдельно: без прежнего ключа подключение из восстановленной БД не расшифруется
([переменные окружения](https://www.metabase.com/docs/latest/configuring-metabase/environment-variables)).
Secrets находятся на диске в открытом виде с ACL; включите BitLocker и храните
копии на зашифрованном независимом носителе. Не включайте secrets в Git, tickets,
скриншоты или логи. Wrapper передаёт значения в окружение дочернего процесса,
не в аргументы или NSSM AppEnvironmentExtra; Telegram-токен удаляется из окружения.

Сначала задайте ограниченный DATABASE_URL в административной сессии и выполните
`bvk-analytics.exe -config config.json -once`; проверьте состояние обновления и
права. Затем запускайте службы по очереди. После проверки `Get-Service` и логов:

```powershell
Start-Service BVKAnalytics
Start-Service BVKMetabase
Invoke-RestMethod http://127.0.0.1:3000/api/health
# Загрузите только ACME_EMAIL/ANALYTICS_DOMAIN из protected JSON в окружение сессии.
C:\BVKAnalytics\bin\caddy.exe validate --config C:\BVKAnalytics\Caddyfile --adapter caddyfile
Start-Service BVKCaddy
Set-Service BVKAnalytics -StartupType Automatic
Set-Service BVKMetabase -StartupType Automatic
Set-Service BVKCaddy -StartupType Automatic
```

NSSM перезапускает упавший процесс с задержкой 10 секунд; это не внешний мониторинг.
Metabase слушает только `127.0.0.1:3000`, административный API Caddy — loopback
2019. Caddy обслуживает HTTPS и проксирует в Metabase. Порты 80/443 должны вести
на сервер для ACME и HTTPS; DNS A/AAAA должны соответствовать реальному маршруту
([HTTPS](https://caddyserver.com/docs/automatic-https), [служба Windows](https://caddyserver.com/docs/running)).
Не открывайте наружу 3000/2019/5432/8080. Если домена/маршрута нет, проверяйте
Metabase локально; внешнее HTTPS остаётся незавершённым этапом внедрения.

## Настройка и приёмка Metabase

Первичную настройку выполните через localhost до публикации: отдельный admin,
два персональных аккаунта руководителей, сложные пароли. Подключите исходную БД
**только** как `bvk_metabase_reader`, выберите схему `analytics`. Прикладное
подключение `bvk_metabase_app` не добавляйте как источник аналитики.

Отключите публичные ссылки, embedding, самостоятельное создание аккаунтов/SSO,
запись Actions, uploads, model persistence и transforms. В OSS не используйте
платные row-level/column-level permissions или serialization. Настройте группу
руководителей: View коллекции опубликованных карточек, аналитические данные для
просмотра/выгрузки, без прав Curate/admin; проверьте суммарные права All Users.
Оба руководителя видят все отделы/компании. Русские названия, единицы, фильтры,
вкладки и явные переходы SQL-карточек — по спецификации панели. Настройте
открытый дашборд на обновление раз в минуту и предупреждение о снимке старше 5 минут.

Проверки под каждым аккаунтом: вход/выход, запрет создания/изменения опубликованных
карточек, выгрузка, совпадение KPI со списком, открытие старой заявки после
удаления Telegram-топика. Из приватного браузера без авторизации нельзя получить
дашборд/данные; API health может быть публичным. Проверьте desktop/mobile HTTPS,
сертификат, перенаправление HTTP, отсутствие внешнего доступа к внутренним портам,
перезапуск Windows, сбой PostgreSQL и сохранение прежнего снимка. Бот проверяйте
отдельно через существующий `http://127.0.0.1:8080/ready`.

## Ежедневная копия и проверка восстановления

`backup-metabase.ps1` останавливает только Metabase на время копии, делает custom
dump `bvk_metabase`, проверяет читаемость `pg_restore --list`, сохраняет конфиг,
версии, wrapper, secrets и import-state.json (если он размещён в InstallDir) с hash-манифестом. Это **не доказательство полного
восстановления**. Копия БД бота/файлов остаётся существующим отдельным процессом.

Создайте задачу Windows Task Scheduler от SYSTEM (ACL конфигурации и копии
допускают SYSTEM); для PostgreSQL используйте отдельную backup-роль
с правом чтения базы настроек. Если вместо SYSTEM используете другую Windows-
учётную запись, настройте доступ к службам, секретам и каталогу копии отдельно. Задайте PGHOST,
PGPORT, PGUSER, PGPASSFILE; файл паролей защитите ACL. Не используйте URL с паролем
в командной строке. Пример ручного запуска:

```powershell
.\scripts\analytics\backup-metabase.ps1 -InstallDir 'C:\BVKAnalytics' -BackupRoot 'E:\BVKBackups\Metabase' -PgBin 'C:\Program Files\PostgreSQL\17\bin'
.\scripts\analytics\restore-metabase.ps1 -BackupDir 'E:\BVKBackups\Metabase\REPLACE' -PgBin 'C:\Program Files\PostgreSQL\17\bin' -TargetDatabase bvk_metabase_restore_drill
```

Restore проверяет hash, создаёт **новую** БД с префиксом `bvk_metabase_restore_`,
восстанавливает одной транзакцией и проверяет таблицы пользователей/карточек/
дашбордов. Уже существующая БД не заменяется. Для drill нужны PostgreSQL права
CREATE DATABASE и SET ROLE `bvk_metabase_app`; runtime роли ими не обладают.
При сбое сохраните отдельную тестовую БД для исследования, после проверки удалите
её вручную. Не назначайте эти повышенные права службам.

Для полного drill запустите той же версией JAR отдельный Metabase на loopback 3001
с `MB_DB_DBNAME` тестовой БД, прежним encryption key и без Caddy. Отключите jobs
через `MB_DISABLE_SCHEDULER=true`, SMTP/webhooks/subscriptions в тестовом экземпляре.
Войдите обоими аккаунтами, сравните коллекции/дашборды/фильтры/переходы/выгрузку,
подключение аналитики и количество объектов. Зафиксируйте дату, hash копии,
время восстановления и результаты. Проверьте независимый носитель и ежедневные
ошибки задач; retention копий и внешнее уведомление настройте отдельно.

При переключении остановите Metabase, измените защищённый `MB_DB_DBNAME` на
проверенную восстановленную БД, восстановите конфигурацию и encryption key,
переназначьте ACL через установочную процедуру, запустите и выполните проверки.
Старую БД сохраняйте для отката; откат версии JAR допускается только с её
совместимой копией прикладной БД, не поверх новых миграций Metabase.

## Второй сервер: только ручное переключение

1. Подготовьте те же закреплённые версии, PostgreSQL, роли, файлы бота и проверенную
   копию исходной/прикладной БД. На резервном сервере службы бота выключены.
2. Остановите и отключите автоматический запуск BVKSupport на первом сервере.
   Если он недоступен, подтвердите его отключение/изоляцию от Telegram и БД.
   Два активных polling-бота недопустимы; advisory lock в одной БД не защищает
   два сервера с разными восстановленными БД.
3. Перенесите последнюю проверенную копию БД/файлов/конфигов/секретов, назначьте
   ACL, сначала выполните штатные проверки бота и один аналитический расчёт.
4. Запустите один бот, аналитику и Metabase, проверьте `/ready`, свежесть,
   вход руководителей, дашборды. Переключите DNS/NAT, выпустите/проверьте TLS.
5. Старый сервер оставьте остановленным; возвращение требует того же порядка
   и переноса актуальных данных. Запишите возможную потерю данных после последней
   копии и фактическое время переключения.

Репликация PostgreSQL, автоматическое failover и резервирование архивных файлов
не реализованы; это отдельный этап после получения второго сервера.
