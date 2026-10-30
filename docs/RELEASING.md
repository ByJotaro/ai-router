# Выпуск релиза

## 1. Проверки перед тегом

```powershell
go vet ./...
go test ./...
# e2e поднимает локальный хаб/шлюз; токен и путь к hub.exe не лежат в репо
$env:AI_ROUTER_HUB_TOKEN = '<hub token>'
$env:AI_ROUTER_HUB_EXE   = '<path to hub.exe>'   # нужен, только если хаб не запущен
powershell -ExecutionPolicy Bypass -File scripts\e2e.ps1
```

CI (`ci.yml`) на каждый push в `main` дополнительно собирает GUI через wails и
проверяет, что копии фронтенда `internal/gui/frontend/dist` и
`cmd/ai-router/frontend/dist` не разошлись.

## 2. Версия

- `cmd/ai-router/main.go`: `var Version = "dev"` — подставляется при сборке
  (`-ldflags "-X main.Version=vX.Y.Z"`), менять вручную не нужно;
- `cmd/ai-router/wails.json`: `info.productVersion` — версия в свойствах exe.

## 3. Скриншоты

Снимаются с демо-данных, без живого конфига. Важно: панель вшита из
`internal/gui/frontend/dist`, поэтому скриншоты должны быть сняты **с того же
коммита**, что и релиз, — иначе README покажет UI, которого нет в теге.

1. открыть `internal/gui/frontend/dist/index.html?demo=1&tab=home` (панель — это
   Wails-окно, отдельного веб-сервера у неё нет; демо-режим работает и на file://);
2. окно 1440×900, тёмная тема;
3. сохранить в `docs/screenshots/<tab>.png`: `home`, `providers`, `logs`
   (в README идут только эти три; остальные вкладки пока не показываем).

## 4. Тег

```powershell
git tag v1.0.0
git push origin v1.0.0
```

Workflow `release.yml` соберёт `ai-router-vX.Y.Z-windows-amd64.exe`, посчитает
SHA-256 и приложит оба файла к GitHub Release вместе с автоматическими notes.

## 5. После релиза

- проверить, что exe запускается на чистой машине (WebView2 Runtime);
- обновить `README.md`, если изменились разделы «Возможности»/«Конфигурация».

## 6. Публикация

Модель: приватная рабочая копия (бета-сборки, эксперименты, вся история) и
публичный репозиторий — **снимок рабочего дерева с чистой историей**. Публичный
собирается экспортом, а не переключением видимости: история приватной копии
наружу не уходит.

### 6.0 Гейты перед экспортом

```powershell
# 1) в дереве нет секретов и личных путей
git grep -nIE "([0-9a-f]{64}|sk-[A-Za-z0-9]{16,}|[A-Z]:/|C:\\\\Users)" -- . ':!*.png' ':!*.ico'
# 2) LICENSE на месте (AGPL-3.0), см. п.4
# 3) CI зелёный на коммите, который экспортируем
# 4) `gh api repos/<owner>/<repo>/license --jq .license.spdx_id` отдаёт AGPL-3.0
#    (любая правка в конце LICENSE ломает автоопределение — держите его чистым)
```

### 6.1 Экспорт дерева (без истории)

```powershell
$S = "<scratch>"
git archive HEAD | tar -x -C $S          # только трекаемые файлы, без .git
# внутренние материалы остаются в приватной копии:
Remove-Item -Recurse -Force $S\docs\gui-audits
Remove-Item $S\docs\pool-identity-recycle.md
Remove-Item $S\.active, $S\extract_js.js
cd $S
git init -b main
git add -A
git commit -m "ai-router: public snapshot"
```

### 6.2 Публикация

```powershell
gh repo create ByJotaro/ai-router --public --source . --push \
  --description "OpenAI-compatible LLM gateway with a desktop control panel (beta)"
gh repo edit ByJotaro/ai-router --add-topic openai,gateway,llm,wails,go
```

Для уже существующего публичного репозитория — обычный `git push` из снимка:
каждый выпуск это один коммит, история остаётся читаемой.

### 6.3 Что в публичный не переносится

- `docs/gui-audits/` — внутренние аудиты вкладок;
- `docs/pool-identity-recycle.md` — измерительные заметки по транспортному пулу;
- рабочая мелочь вроде `.active`, `extract_js.js`;
- реальные значения токенов (`admin_token`, `hub.token`) — в дереве только
  плейсхолдеры; e2e берёт токен из окружения.
