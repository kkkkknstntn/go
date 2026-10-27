# Последний билет — бронирование и покупка мест

Сервис на Go + PostgreSQL: бронь места с TTL, покупка с отдельным payment timeout, оплата переводом
между тестовыми балансами пользователя и организатора. Корректность держится на блокировках в БД, а не
на памяти процесса, поэтому несколько экземпляров приложения дают те же гарантии.

> Подробное руководство по проекту (запуск, структура, объяснение Go-конструкций для Java-разработчиков,
> путь запроса, тесты, отладка): [docs/DEVELOPER_GUIDE.md](docs/DEVELOPER_GUIDE.md).

## Запуск

```bash
docker compose up -d                       # PostgreSQL 16 на :5432 (БД tickets)
go run .                                   # API на :8080, Swagger UI: http://localhost:8080/docs
```

При старте применяется схема (идемпотентно, под advisory lock — безопасно для нескольких экземпляров) и
создаются демо-данные: организатор `concert-company` (баланс 50 000) и `event-1` с местами A1..A100
(A1–A80 по 2000, A81–A100 по 2500). Отключить: `SEED=false`.

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/tickets?sslmode=disable` | PostgreSQL |
| `LISTEN_ADDR` | `:8080` | адрес HTTP |
| `RESERVATION_TTL` | `5m` | время жизни брони |
| `PAYMENT_TIMEOUT` | `10m` | время на оплату после начала покупки |
| `WORKER_INTERVAL` | `5s` | период воркеров |
| `SHUTDOWN_TIMEOUT` | `15s` | ожидание активных запросов при остановке |
| `DB_MAX_CONNS` | по умолчанию pgxpool | размер пула |
| `SEED` | `true` | создать демо-данные |

Два экземпляра: `LISTEN_ADDR=:8081 go run .` и `LISTEN_ADDR=:8082 go run .` на одной БД.

### Тесты

```bash
docker exec <postgres-container> createdb -U postgres tickets_test   # отдельная БД, чтобы не мусорить в демо
TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/tickets_test?sslmode=disable' go test ./...
TEST_DATABASE_URL='...' go test -race ./...
```

Без `TEST_DATABASE_URL` используется БД `tickets`. Если PostgreSQL недоступен, тесты печатают `SKIP` и
завершаются успешно — это не означает, что они прошли, смотрите на строку SKIP.

### Пример сценария

```bash
curl -XPOST localhost:8080/users -d '{"id":"user-1","name":"Ivan","balance":10000}'
curl localhost:8080/events/event-1/seats
curl -XPOST localhost:8080/reservations -d '{"userId":"user-1","eventId":"event-1","seatId":"A17"}'   # -> reservationId
curl -XPOST localhost:8080/reservations/<rid>/purchase -d '{"userId":"user-1"}'                       # -> paymentId, amount=2000
curl -XPOST localhost:8080/payments/<pid>/confirm                                                     # деньги переходят организатору
curl localhost:8080/users/user-1; curl localhost:8080/organizers/concert-company
curl localhost:8080/history/<pid>                                                                     # история переходов
```

Для демонстрации TTL: `RESERVATION_TTL=10s PAYMENT_TIMEOUT=20s WORKER_INTERVAL=1s go run .`.

## Архитектура

```
main.go                  конфиг, миграции, сид, воркеры, graceful shutdown
internal/httpapi         HTTP-хендлеры, маппинг ошибок в статусы, OpenAPI (openapi.json) и /docs
internal/service         вся бизнес-логика и SQL (schema.sql — схема)
```

`Client → [Go instance 1 | Go instance 2] → PostgreSQL`. Приложение не хранит состояния: ни кэшей, ни
`sync.Mutex` вокруг броней. Всё время считается часами БД (`clock_timestamp()`), поэтому расхождение
часов между экземплярами ни на что не влияет.

## Структура БД

```
organizers(id, name, balance>=0)          users(id, name, balance>=0)
events(id, name, organizer_id→organizers)
seats(event_id→events, number, price>0, status, ord)       PK(event_id, number)
reservations(id, user_id, event_id+seat_number→seats, status, created_at, expires_at, finished_at)
purchases(id, reservation_id UNIQUE→reservations, status, started_at, finished_at)
payments(id, purchase_id UNIQUE→purchases, user_id, organizer_id, amount>0, status,
         failure_reason, created_at, expires_at, completed_at)
transitions(id, entity, entity_id, from_status, to_status, reason, at)       -- история
```

Связи: Organizer → Event → Seat → Reservation → Purchase → Payment; User → Reservation/Payment.
Цена живёт только в `seats.price`; сумма платежа берётся из неё на сервере, клиентские поля, которых нет в
контракте (в том числе `price`), отклоняются как 400.

Ограничения БД как последняя линия обороны: `CHECK (balance >= 0)` у users и organizers (баланс физически не
может уйти в минус) и частичный уникальный индекс `reservations_one_live_per_seat` на
`(event_id, seat_number) WHERE status IN ('reserved','payment_pending','purchased')` — вторую «живую» бронь на
место БД не примет даже при ошибке в коде.

## Модель состояний

```
seat:         available → reserved → payment_pending → purchased
                  ↑___________|____________|
reservation:  reserved ──cancel──────────────→ canceled
                 │ ──── expire ──────────────→ expired
                 └── purchase → payment_pending ─┬─ confirm ──→ purchased
                                                 ├─ fail / недостаточно денег → payment_failed
                                                 └─ timeout ──────────────────→ expired
purchase:     payment_pending → purchased | failed | timed_out
payment:      pending         → succeeded | failed | timed_out
```

Сверх минимума ТЗ в `reservation` добавлены `purchased` (бронь превратилась в покупку; нужно, чтобы
различать «нельзя отменить purchased») и `payment_failed` (оплата не прошла). Таймаут оплаты завершает
бронь статусом `expired`. Финальные состояния не меняются: все переходы описаны `UPDATE` с проверкой
текущего статуса под блокировкой, обратных переходов в коде нет.

Место освобождается после: cancel, expire, payment failed (в т.ч. insufficient_funds), payment timed_out.
После `purchased` — остаётся занятым навсегда.

## Reservation TTL и payment timeout

* `reserved`: действует `expires_at = now + RESERVATION_TTL`.
* `POST /reservations/{id}/purchase` проверяет `status = reserved AND now < expires_at` и владельца. После
  перехода в `payment_pending` TTL брони **не применяется**: воркер выбирает только `status='reserved'`, а
  бизнес-операции смотрят на `payments.expires_at = now + PAYMENT_TIMEOUT`.
* Задержанный confirm (бронь «истекла» по TTL, но платёж в пределах таймаута) успешен
  (`TestDelayedConfirm`). Опоздавший confirm (после `payments.expires_at`) отклоняется
  (`TestLateConfirm`, `TestLateConfirmWithoutWorker`).

## Модель денег

Баланс — число в `users.balance` / `organizers.balance` (целые, без копеек). Деньги двигаются только при
переходе payment `pending → succeeded`: `users.balance -= amount`, `organizers.balance += amount` в одной
транзакции. fail, timeout, late confirm, duplicate confirm деньги не трогают.

## Границы транзакций

| Операция | Одна транзакция включает |
|---|---|
| Reserve | проверка пользователя, `SELECT … FOR UPDATE` места, ленивое истечение просроченной брони, INSERT reservation, `seats.status='reserved'`, история |
| StartPurchase | lock места и брони, проверка владельца/статуса/TTL, INSERT purchase + payment, reservation и seat → `payment_pending` |
| Confirm | lock места и платежа, проверка статуса и дедлайна, lock пользователя, проверка баланса, списание, зачисление, payment → succeeded, purchase/reservation/seat → purchased |
| Cancel / Expire / Timeout / Fail | lock места и брони/платежа, смена статусов брони, покупки, платежа и места вместе |

Любая ошибка (в том числе отмена `context` клиентом) откатывает транзакцию целиком. Отказные исходы, которые
нужно сохранить (`insufficient_funds`, `timed_out` при confirm, ленивое `expired`), фиксируются коммитом, и
только после него вызывающему возвращается ошибка.

## Защита от двойной брони

Механизм — **пессимистическая блокировка строки места**. Любая операция над местом начинается с
`SELECT … FROM seats WHERE event_id=? AND number=? FOR UPDATE`. Решение «место принадлежит этому
пользователю» принимается атомарно: под этой блокировкой читается статус, и в той же транзакции вставляется
бронь и ставится `seats.status='reserved'`. Конкурентные Reserve встают в очередь на строке; первый
фиксирует бронь, остальные после снятия блокировки видят `reserved` и получают 409. Блокировка — на уровне
PostgreSQL, поэтому два экземпляра приложения сериализуются так же, как две горутины одного.

Почему не `SELECT → INSERT` без блокировки: два читателя оба увидели бы `available`. Почему не optimistic
locking: при 100 соперниках на одно место он давал бы 99 повторов; здесь очередь на блокировке короче и
проще. Второй барьер — частичный уникальный индекс (см. выше).

## Защита от double spending

В `Confirm` строка пользователя берётся `SELECT balance FROM users WHERE id=? FOR UPDATE`, **баланс
проверяется только после** получения блокировки, затем идёт `UPDATE balance = balance - amount`. Два
конкурентных confirm разных платежей одного пользователя блокируются на его строке: второй видит уже
уменьшенный баланс и получает `insufficient_funds`. Баланс не может уйти в минус и по `CHECK (balance >= 0)`.

Порядок блокировок во всех операциях один: **место → резервация/платёж → пользователь → организатор**.
Циклов ожидания нет, дедлоков нет.

## Идемпотентность confirm

Под блокировкой платежа читается его статус. `succeeded` → возвращается тот же результат, 200, деньги не
двигаются, вторая покупка не создаётся. `failed` / `timed_out` → 409. Конкурентные дубликаты сериализуются
блокировкой места, первый переводит деньги, остальные видят `succeeded` (`TestDuplicateConfirm`: 20
параллельных confirm — организатор получил сумму один раз).

## Воркеры, истечение и перезапуск

`RunWorkers` каждые `WORKER_INTERVAL` делает два прохода: брони `status='reserved' AND expires_at <= now()` →
`expired`, платежи `status='pending' AND expires_at <= now()` → `timed_out` (вместе с покупкой и бронью,
место свободно). Каждая запись обрабатывается в своей транзакции под блокировкой места с перепроверкой
условия, поэтому воркеры нескольких экземпляров и параллельные пользовательские операции не мешают друг другу.

**Корректность не зависит от воркера.** Все проверки используют `expires_at <= now` напрямую:
* Reserve над просроченной, но не обработанной бронью лениво переводит её в `expired` и занимает место;
* StartPurchase / Cancel просроченной брони → 409 `reservation_expired` и сохранение `expired`;
* Confirm / Fail после дедлайна → 409 `payment_timed_out` и сохранение `timed_out`;
* `GET /events/{id}/seats`, `GET /reservations`, `GET /payments`, `GET /purchases` показывают «эффективный»
  статус (просроченное `reserved` — `expired`/`available`, просроченное `pending` — `timed_out`).

Всё состояние в PostgreSQL, поэтому после рестарта ничего не теряется, а просроченное не считается
активным (`TestRestart`: новый `Service` без прохода воркера не даёт «ожить» ни брони, ни платежу).

## Ответы на вопросы защиты

* **Где решается, что место принадлежит пользователю?** В транзакции `Reserve` под `FOR UPDATE` на строке места.
* **Почему два экземпляра не продадут место дважды?** Блокировка и уникальный индекс живут в PostgreSQL,
  а не в процессе (`TestTwoInstancesSameSeat`, ручной прогон на :8081/:8082: 1×201 и 99×409).
* **Почему два платежа не потратят баланс дважды?** Блокировка строки пользователя + проверка баланса после
  неё + `CHECK (balance >= 0)`.
* **confirm одновременно с timeout?** Оба берут блокировку места; побеждает первый. Если timeout — confirm
  увидит `timed_out` и получит 409, деньги целы. Если confirm — воркер увидит `succeeded` и ничего не делает.
* **Повторный confirm?** Видит `succeeded`, ничего не переводит.
* **Деньги списались, а начисление организатору упало?** Оба UPDATE и смена статусов — одна транзакция,
  ошибка откатывает всё: либо произошло всё, либо ничего.

## Hotspots и contention

* *Последнее место, 1000 клиентов:* все ждут `FOR UPDATE` на одной строке `seats`. Ожидание линейно, но
  каждая транзакция короткая (одна проверка + одна вставка); получившие блокировку после победителя сразу
  видят `reserved` и откатываются с 409. Конфликтов записи и повторов нет.
* *100 confirm на один баланс:* ждут блокировки строки `users`. Каждый, получив её, видит актуальный
  баланс: первые `balance/price` успешны, остальные завершаются `insufficient_funds` (`TestBalanceHotspot`:
  30 платежей по 100 при балансе 1000 → ровно 10 успешных, баланс 0).
* *Строка организатора* — общий hotspot всех покупок одного мероприятия: она блокируется последней и
  ненадолго; при очень больших нагрузках её можно заменить на append-only ledger (вне рамок задания).
* Ожидающая транзакция держит соединение пула; при очень большом числе одновременных запросов их потолок —
  `DB_MAX_CONNS`. Дедлоков нет из-за единого порядка блокировок.

## Результаты конкурентных тестов

Прогон `go test -race ./...` на PostgreSQL 16 (Docker): все тесты проходят.

| Тест | Результат |
|---|---|
| Эксперимент 1: 100 параллельных reserve одного места | 1 успех, 99 конфликтов, в БД одна `reserved` бронь |
| 60 reserve через два пула/«экземпляра» | 1 успех |
| Эксперимент 2: баланс 2000, два confirm по 2000 (20 повторов) | всегда 1 `succeeded` + 1 `insufficient_funds`; user = 0, org +2000 |
| Эксперимент 3: expire vs purchase на границе TTL (60 повторов) | каждый раз ровно одно состояние: 48× `expired`, 12× `payment_pending`; ни разу «оба» |
| Эксперимент 4: timeout vs confirm на дедлайне (60 повторов) | 45× `timed_out` (деньги целы), 15× `purchased` (деньги переведены); ни разу `timed_out` + списание |
| cancel vs purchase (40 повторов) | всегда ровно `canceled` или `payment_pending` |
| 20 параллельных duplicate confirm | деньги переведены один раз |

Соотношение исходов в гонках зависит от тайминга и от запуска к запуску меняется; важен инвариант.

Покрыты также: reserve, cancel, expiration (в т.ч. без воркера), start purchase, успешная оплата,
недостаток средств, fail, payment timeout, delayed/late/duplicate confirm, рестарт, остановка воркера по
`context`, откат при отменённом `context`, HTTP-сценарии (409, 402, 400, 404).

## Context и graceful shutdown

Все обращения к БД принимают `context.Context` из запроса; транзакции откатываются при любой ошибке, в том
числе при отмене контекста. По `SIGINT` / `SIGTERM`:

1. `http.Server.Shutdown` — новые запросы не принимаются;
2. активные запросы доделываются в пределах `SHUTDOWN_TIMEOUT`, по его исходу оставшиеся обрываются
   (их транзакции откатываются);
3. воркеры останавливаются по отмене контекста, дожидается завершения текущего прохода;
4. незавершённые транзакции откатываются (`defer Rollback`);
5. пул соединений закрывается;
6. процесс завершается.

## API

Swagger UI: `/docs`, спецификация: `/openapi.json`. Дополнительно к минимальному набору ТЗ: `POST /organizers`,
`GET /organizers/{id}`, `POST /events` (список мест или `generate: {prefix,count,price}`),
`GET /history/{id}` (история переходов reservation / purchase / payment). `POST /users`, `/organizers`,
`/events` принимают необязательный `id` (удобно для демонстрации: `user-1`).

Коды ошибок: 400 неверный запрос, 403 бронь другого пользователя, 404 не найдено, 409 место занято /
недопустимый переход / бронь истекла / оплата просрочена / уже существует, 402 недостаточно средств
(в теле — сохранённый `payment` со статусом `failed`).

Авторизации нет (по ТЗ), `/payments/{id}/confirm` и `/fail` — тестовые, доступны всем.
