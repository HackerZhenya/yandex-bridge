# yandex-bridge

Мост между Яндекс Умным Домом и Apple HomeKit. Крутится в Docker на Raspberry Pi
в домашней сети, читает устройства через
[пользовательский API Яндекса](https://yandex.ru/dev/dialogs/smart-home/doc/ru/concepts/platform-protocol)
и отдаёт их в HomeKit через HAP.

Главная цель — **стабильность**. Аксессуары не должны отваливаться со временем,
а если что-то всё же сломалось, об этом должно прийти уведомление в Apple Home,
а не тишина.

---

## Что здесь сделано иначе

Референсный проект [Mon4ik/yandex-homekit](https://github.com/Mon4ik/yandex-homekit)
(архивирован 24.07.2025) со временем терял аксессуары. Две причины, обе закрыты
здесь и обе покрыты тестами-регрессиями.

### 1. Стабильные accessory id

HomeKit опознаёт аксессуар внутри моста по числовому `aid` и **запоминает** его:
на этот номер завязаны комната, имя, сцены и автоматизации. В
[`brutella/hap`](https://github.com/brutella/hap/blob/master/server.go)
функция `Server.add()` раздаёт `aid` последовательно **по порядку слайса**, если
`a.Id == 0`:

```go
aid := uint64(1)
for _, a := range as {
    if a.Id == 0 {
        a.Id = aid
        aid++
    }
    ...
}
```

Если строить аксессуары прямо из массива `devices` ответа `/v1.0/user/info`, то
достаточно, чтобы Яндекс переставил устройства местами или не вернул одно из них
— и все последующие `aid` сдвинутся. HomeKit увидит за старыми номерами другие
аксессуары и выбросит их вместе с настройками пользователя.

Здесь каждое устройство получает `aid` один раз, навсегда, и назначение
пишется в `/data/aids.json` **до** того, как будет использовано. Освобождённые
номера никогда не переиспользуются. См. [`registry.go`](internal/bridge/registry.go)
и тесты в [`registry_test.go`](internal/bridge/registry_test.go).

### 2. Набор аксессуаров не перестраивается по сбойному опросу

`hap` фиксирует список аксессуаров в момент `NewServer`, поэтому добавление или
удаление устройства требует перезапуска HAP-сервера. Это заметная для
пользователя операция, и запускать её по неполному ответу API нельзя.

Правило: топология меняется только после **N подряд успешных** опросов, которые
согласны с новой картиной (по умолчанию 3). Неудачный опрос до этой логики
вообще не доходит. См. [`supervisor.go`](internal/bridge/supervisor.go).

### 3. Ротация refresh-токена

Яндекс выдаёт **новый refresh-токен при каждом обновлении**, аннулируя
предыдущий. Если сохранить его неатомарно или упасть между обновлением и
записью — доступ потерян навсегда, и починить это можно только повторной
авторизацией.

Здесь токен пишется через temp-файл + `fsync` + `rename` + `fsync` каталога,
предыдущая версия сохраняется в `.bak`, и новый refresh-токен оказывается на
диске **до** того, как новый access-токен отдан вызывающему коду. Плюс
обновление за 30 дней до истечения, так что до дедлайна дело не доходит вовсе.

---

## Быстрый старт

### 1. Зарегистрировать приложение в Яндексе

1. Открыть <https://oauth.yandex.ru/client/new>.
2. Платформа — **Другое устройство или сервис**.
3. Права доступа: **`iot:view`** и **`iot:control`** (Умный дом).
4. Сохранить `ClientID` и `Client secret`.

Callback URL не нужен: используется device flow, при котором код вводится на
<https://ya.ru/device>.

### 2. Настроить окружение

```bash
git clone <repo> && cd yandex-bridge
mkdir -p data && sudo chown -R 10001:10001 data
cp config.example.yaml data/config.yaml   # необязательно
```

Создать `.env` рядом с `compose.yaml`:

```dotenv
YANDEX_CLIENT_ID=ваш_client_id
YANDEX_CLIENT_SECRET=ваш_client_secret

# Восемь цифр. Придумайте свой; HomeKit отвергает тривиальные вроде 12345678.
HOMEKIT_PIN=010-20-030

# Интерфейс, на котором анонсировать мост. См. раздел про сеть ниже.
HOMEKIT_INTERFACES=eth0

TZ=Europe/Moscow
```

### 3. Запустить и авторизоваться

```bash
docker compose up -d && docker compose logs -f
```

В логах появится:

```
action required: authorize this bridge with Yandex  url=https://ya.ru/device user_code=1234567
```

Тот же код доступен по HTTP, если смотреть в логи неудобно:

```bash
curl -s http://raspberrypi.local:8080/healthz | jq .auth
```

Открыть <https://ya.ru/device>, ввести код, подтвердить доступ. Код живёт
5 минут; если не успели — мост сам запросит новый и напечатает его.

### 4. Добавить мост в Apple Home

Дом → **+** → Добавить аксессуар → **Нет кода или не удаётся отсканировать?** →
мост появится в списке. Ввести тот же PIN, что в `HOMEKIT_PIN`.

Устройства подтянутся автоматически.

---

## Сеть: почему `network_mode: host` обязателен

HomeKit находит аксессуары через mDNS — multicast UDP на `224.0.0.251:5353` с
TTL = 1. Эти пакеты **не проходят через NAT docker-сети**, поэтому в любом
режиме кроме `host` iPhone просто не увидит мост, и пейринг никогда не
предложит его.

Два следствия:

1. Контейнер делит порты с хостом — `51826` и `8080` должны быть свободны.
2. Если на Pi запущен `avahi-daemon`, он занимает `5353` и конфликтует со
   встроенным в `hap` dnssd-респондером:

   ```bash
   ss -ulpn | grep 5353
   ```

   Если avahi там есть и мост не появляется в Доме — остановить его
   (`sudo systemctl disable --now avahi-daemon`) либо не запускать мост на этом
   хосте.

Отдельно про `HOMEKIT_INTERFACES`: анонс одновременно на `eth0`, `wlan0` и
`docker0` — известная причина, по которой HomeKit теряет мост. Указывайте один
интерфейс, тот, в котором живут ваши Apple-устройства.

---

## Что попадает в HomeKit

По умолчанию экспортируется всё поддерживаемое, настройка не нужна.

| Yandex | HomeKit |
|---|---|
| `devices.types.light` (+ подтипы) | Lightbulb: On, Brightness, Hue/Saturation **или** ColorTemperature |
| `devices.types.socket` | Outlet |
| `devices.types.switch` (+ `switch.relay`) | Switch |
| `devices.types.sensor.climate` и любое устройство со свойствами | TemperatureSensor / HumiditySensor / BatteryService |
| неизвестный тип, но есть `on_off` | Switch |

Датчики температуры, влажности и заряда добавляются **дополнительными сервисами
к тому же аксессуару**, а не отдельными плитками: розетка с термометром остаётся
одной розеткой.

**Цвет или температура, но не оба.** Apple Home ведёт себя непредсказуемо, когда
у лампы одновременно есть `ColorTemperature` и `Hue`/`Saturation` — на это
жаловался и референсный проект. Мост выбирает одно: цвет, если лампа его
умеет. Переопределяется через `color_mode`.

### Переопределения

Файл `data/config.yaml` нужен только чтобы что-то скрыть, переименовать или
поменять тип. Полный пример — [`config.example.yaml`](config.example.yaml).

```yaml
devices:
  # Неумный вентилятор в умной розетке: в HomeKit это вентилятор,
  # а не розетка, — Siri и Дом относятся к нему соответственно.
  "socket-id-xxx":
    type: fan
    name: "Вентилятор в спальне"

  "lamp-id-yyy":
    color_mode: temperature   # hsv | temperature

  "device-id-zzz":
    exclude: true
```

Найти `device_id` проще всего так:

```bash
LOG_LEVEL=debug docker compose up
```

---

## Когда что-то ломается

### Аксессуар «Bridge Health»

Отдельный аксессуар с датчиком открытия: **«Закрыт» = всё хорошо, «Открыт» =
проблема**. Датчик открытия выбран специально — Apple Home умеет слать по нему
пуш-уведомления штатно, включается в приложении, кода не требует.

Настроить один раз: Дом → аксессуар «Yandex Bridge Health» → Настройки →
Уведомления → включить.

Взводится при: мёртвом refresh-токене, нескольких подряд неудачных опросах,
устойчивых 5xx от Яндекса.

Рядом лежит переключатель **Re-authenticate** — включить, чтобы заново запустить
device flow, не заходя на Pi по SSH. Код появится в логах и в `/healthz`.

### «Нет ответа» на устройствах

Если Яндекс недоступен или вернул `DEVICE_UNREACHABLE`, мост возвращает
HomeKit код `-70402`, и устройство показывается серым с «Нет ответа» — вместо
того, чтобы молча показывать устаревшее состояние как актуальное.

Одно недоступное устройство при этом **не** считается сбоем связи с Яндексом.

### `/healthz`

```bash
curl -s http://raspberrypi.local:8080/healthz | jq
```

```json
{
  "status": "ok",
  "uptime": "36h12m4s",
  "auth": { "state": "ok", "token_expiry": "2027-05-14T09:21:00Z" },
  "yandex": { "reachable": true, "consecutive_failures": 0,
              "last_successful_poll": "2026-07-31T12:03:11Z" }
}
```

Возвращает `503`, когда мост нездоров, — на это завязан docker healthcheck.

---

## Устройство проекта

```
cmd/yandex-bridge/      wiring, сигналы
internal/
  atomicfile/           запись файлов, переживающая отключение питания
  auth/                 device flow, атомарное хранение токена, ротация refresh
  bridge/
    registry.go         стабильные aid                    ← ключевой фикс
    supervisor.go       graceful restart HAP-сервера      ← ключевой фикс
    spec.go             yandex device → форма аксессуара
    accessory.go        сборка HomeKit-аксессуаров, конвертация величин
    sync.go             опрос, диффинг, запись
    health.go           аксессуар Bridge Health
  config/               env + YAML
  logging/              slog + мост из hap/log
  status/               /healthz
  yandex/               типизированный клиент API
```

### Почему свой клиент Яндекса

[`SkurkovPavel/go-yahome`](https://github.com/SkurkovPavel/go-yahome) не подошёл:
в [`iot/devices.go`](https://github.com/SkurkovPavel/go-yahome/blob/main/iot/devices.go)
`SetActionToDevice` отправляет `nil` вместо тела запроса — управление
устройствами не работает; `QueryDevices` бьёт в endpoint провайдера, а не
пользовательский; состояния умений не типизированы; тело ответа при ошибке
теряется; нигде нет `context.Context`. Пользовательский API — шесть
endpoint'ов, свой клиент занимает ~350 строк.

### Почему свой device flow

`golang.org/x/oauth2/yandex` содержит только `AuthURL` и `TokenURL`, без
`DeviceAuthURL`. И device flow Яндекса не соответствует RFC 8628:
`Config.DeviceAccessToken` шлёт
`grant_type=urn:ietf:params:oauth:grant-type:device_code` и `device_code=...`,
а Яндекс ждёт `grant_type=device_code` и `code=...`. Обновление токена
стандартное, поэтому эта половина идёт через `x/oauth2`.

---

## Разработка

Go на машине не обязателен — всё собирается в Docker:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go build ./...
```

Тесты с race-детектором (нужен cgo, поэтому образ с gcc):

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "apk add --no-cache gcc musl-dev >/dev/null && CGO_ENABLED=1 go test ./... -race -count=1"
```

Сборка образа под Pi 5:

```bash
docker build --platform linux/arm64 -t yandex-bridge:latest .
```

### Проверка ключевых инвариантов

Два теста прямо соответствуют багу, из-за которого отваливались аксессуары:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine \
  go test ./internal/bridge/ -run 'TestAIDs|TestTransientTopology' -v
```

- `TestAIDsSurviveRestart`, `TestAIDsAreStableWhenYandexReordersDevices`,
  `TestAIDsAreStableWhenADeviceGoesMissing` — номера аксессуаров не плывут.
- `TestTransientTopologyChangeIsIgnored` — мигающее устройство не приводит к
  пересборке.

### Ручная проверка на Pi

1. Запомнить `data/aids.json`, перезапустить контейнер несколько раз, добавить и
   удалить устройство в Яндексе — `aid` существующих устройств не изменились,
   комнаты и автоматизации в Доме на месте.
2. Заблокировать `api.iot.yandex.net` — устройства становятся «Нет ответа»,
   Bridge Health переходит в «Открыт», в логах видны ретраи с backoff. Вернуть
   сеть — всё восстанавливается без перезапуска.
3. Испортить `refresh_token` в `data/token.json`, перезапустить — мост не падает
   в crash-loop, взводит Bridge Health и печатает новый код.

---

## Что не поддерживается

Осознанно вне первой версии: термостаты и кондиционеры, увлажнители и
очистители, шторы, замки и клапаны (`openable.*`), медиаустройства, камеры,
пылесосы, сценарии и группы. Модель расширяемая — добавление типа это новая
ветка в [`kindFor`](internal/bridge/spec.go) и сборщик в
[`accessory.go`](internal/bridge/accessory.go).

Устройства неизвестного типа, у которых есть `on_off`, экспортируются как
Switch; всё остальное пропускается, а не угадывается.
