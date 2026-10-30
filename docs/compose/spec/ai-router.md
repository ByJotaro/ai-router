# ai-router — спека модуля

> **Историческая спека (Phase 1, 2026-09).** Актуальное описание — `README.md`
> в корне: там верные пути (`cmd/ai-router`, панель внутри того же exe), SQLite
> `state.db` вместо `state.json` и текущий CLI. Ниже сохранён исходный контракт
> хаба; расхождения с README считать устаревшими.

Статус: реализовано (Phase 1). Репозиторий: `github.com/ByJotaro/ai-router`, ветка `feat/ai-router`.

## Назначение

ai-router — отдельный Go-проект: OpenAI-совместимый LLM-шлюз (`:9001`) и модуль
хаба `ai-router` (действия на `:9102`). Стек: Go; конфиг
`%LOCALAPPDATA%\ai-router\config.yaml`, состояние — SQLite `state.db`.

## Контракт OpenAI

- `POST /v1/chat/completions`:
  - non-stream: тело upstream ретранслируется как есть (`choices`, `usage`);
  - `stream: true`: SSE-passthrough — `data: {chunk}`, финальный chunk с
    `usage`, завершение `data: [DONE]` (добавляется, если upstream его не прислал);
    если upstream не умеет SSE, ответ конвертируется в один chunk + usage + [DONE];
  - `tools`/`tool_calls` проходят прозрачно;
  - ошибки: `{error:{message, type, code}}` —
    `unauthorized` (401), `rate_limit` (429), `forbidden` (403),
    `invalid_request` (400), `model_not_found` (404), `upstream_error` (502).
- `GET /v1/models` — `{object:"list", data:[{id, object:"model", owned_by}]}`;
  перечисляются модели провайдеров с явным списком (wildcard-провайдеры не
  рекламируются).

## Аутентификация

- admin-токен (`config.yaml: admin_token`) — полный доступ;
- виртуальные ключи (`keys[]`) — квоты (tokens/reqs, 0 = безлимит) и allowlist
  моделей; исчерпание → 429 `rate_limit_error`.

## Провайдеры и маршрутизация

```yaml
providers:
  gitlawb:
    type: openai-compatible      # oauth — зарезервирован
    base_url: https://opengateway.gitlawb.com/v1
    api_keys: [ogw_...]          # пул ключей
    models: [mimo-v2.5-pro]
    fallbacks: [openrouter]      # цепочка при сбое
    timeout_s: 300
    egress: ""                   # опц. прокси-URL
```

- Пулы ключей: least-used (по умолчанию) или round-robin; после 3 неудач (5xx)
  ключ отключается на 60 с; 401/429 — немедленный переход к следующему ключу.
- Fallback: порядок кандидатов детерминирован (сортировка по имени провайдера,
  затем явные `fallbacks`, циклы отсекаются). Транспортные ошибки и 401/429 →
  следующая ключ/провайдер; 5xx → следующий провайдер цепочки.
- Провайдер с пустым `base_url` пропускается при старте (предупреждение в лог).
- Egress: `providers.<name>.egress` — HTTP-прокси для выхода.

## Модуль хаба

- Регистрация: `sdk.Client.Register` (base_url = `http://<hub.addr>`);
  токен модуля берётся из `hub.token`; ошибка регистрации не блокирует сервис
  (пред-объявленный модуль или недоступный хаб).
- Слушает `/health` + `/actions/{name}` на `hub.addr` (по умолчанию
  `127.0.0.1:9102`), Bearer = токен модуля.
- Действия:
  - `chat.complete {model, messages, max_tokens?, temperature?}` → live-запрос
    через маршрутизатор, результат: content/tool_calls/usage + provider/latency_ms;
  - `models.list` → модели;
  - `keys.list` → виртуальные ключи (маскированные);
  - `usage` → usage по ключам + суммарно;
  - `test {model?}` → пробный запрос к первому доступному модель-провайдеру.
- События: `ai.request` (после каждого успешного чата через gateway),
  `ai.fallback` (при переключении провайдера/ключа).

## Импорт (cmd/import-opencode)

Источник: opencode `auth.json` + `opencode.jsonc`. apikey-строки → `api_keys[]`
(известные baseURL — автоматически), oauth-строки — только отчёт. Ключи не
логируются.

## E2E (scripts/e2e.ps1)

Фейк-upstream'ы + демон на изолированных портах + хаб. Покрытие: models,
non-stream, стриминг ([DONE]+usage), fallback (500 → второй провайдер), tools,
401/404/429, онлайн-статус модуля в хабе, действия hub `chat.complete/test/usage`.
Безопасность: скрипт останавливает только собственные процессы (по PID).

## Известные ограничения

- oauth-провайдеры не импортируются (нет endpoint/моделей в конфиге).
- Провайдеры без baseURL требуют ручного заполнения.
- Виртуальные ключи управляются только через `config.yaml` (CLI key-add не
  реализован; есть `keys`/`usage` чтение).

## v2 — ядро: устойчивость, роутинг, квоты

- **3-слойная устойчивость**: circuit breaker per-provider (internal/breaker, closed/open/half_open, lazy recovery, персист в SQLite `breakers`), cooldown ключей (internal/pool: экспоненциальный backoff base*factor^level, cap, jitter ±20%, Retry-After, rate_limited_until, single-key never disabled), model lockout (internal/lockout).
- **Роутинг** (internal/routing): каскад тиров subscription→api→cheap→free + стратегии priority|weighted|round-robin|p2c|cost|lkgp; дефолт priority = детерминированный порядок.
- **Квоты** (internal/keys): SQLite-бэкенд (state.json мигрируется одноразово), per-key token/request/USD-бюджеты, fair-share per (key,model), no_log, шифрование ключей AES-256-GCM enc:v1 (`STORAGE_ENCRYPTION_KEY`).
- **Санитизация** (internal/crypto): денyлист заголовков вверх, SafeError вниз.
- **Телеметрия**: заголовки X-AiRouter-Provider/Key/Model/Latency-Ms; идемпотентность (X-Request-ID/body hash, TTL 5с, non-stream).
- **Hub-поверхность**: + keys.create/keys.revoke/quota.set/routes.status/models.sync; события ai.quota_exhausted/ai.breaker_open/ai.lockout; routes.status отдаёт состояние брейкеров/lockout для GUI.
- **Persist**: SQLite `%LOCALAPPDATA%\ai-router\state.db` (WAL): breakers, pool_keys, keys, quota, usage, models_catalog, audit, idempotency.

## GUI ендпоинта (сейчас: панель внутри `cmd/ai-router`)

Wails-окно + systray-трей поверх ядра (отдельная команда, свои экземпляры
роутера/ключей из того же `config.yaml` и `state.db` — состояние брейкеров и
пулов персистится, GUI видит то же, что сервер).

```powershell
# сборка (статический фронт, npm не нужен):
cd cmd/ai-router
wails build            # → build/bin/ai-router.exe

# запуск: панель одним процессом с демоном; --cli = headless
ai-router.exe serve [-config path]     # окно панели, демон в том же процессе
ai-router.exe serve --cli              # только демон
```

> Историческая часть ниже описывает отдельный `omnigate-gui.exe`; после слияния
> его роль играет окно `cmd/ai-router`.

- Трей на energye/systray (форк getlantern с `SetOnClick`): **ЛКМ по иконке
  открывает окно**, ПКМ — меню «Открыть/Тест/Выйти» (Тест — живой
  chat.complete на первой модели, результат в лог).
- Окно: панель ядра — KPI-карточки (провайдеры/модели/ключи/расход),
  карточки ендпоинтов (base_url, модели, статус пула ключей, бэкофф,
  disabled, egress, timeout), таблицы брейкеров и lockout, ключи с квотами
  (токены/запросы/бюджет USD — использовано/лимит/проценты), итоги
  использования, «Живой тест» (chat.complete через роутер). Автообновление
  5 с.
- Биндинги (window.go.main.App.*): `Status`, `Endpoints`, `Breakers`,
  `Lockouts`, `Keys`, `Usage`, `Test(model)`.
- Хаб: бейдж online/offline по публичному `GET {hub.url}/v1/health` (без
  токена).
