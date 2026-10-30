# Последний билет — бронирование и покупка мест

Сервис на Go и PostgreSQL. Бронь места с TTL, покупка с отдельным таймаутом оплаты, оплата как атомарный перевод
с тестового баланса покупателя на баланс организатора. Авторизация на JWT. Гарантии держатся на блокировках
PostgreSQL, а не на памяти процесса, поэтому несколько экземпляров приложения дают те же гарантии, что и один.

- Руководство для разработчика (как устроен Go-проект, путь запроса, отладка; с аналогиями из Java):
  [docs/DEVELOPER_GUIDE.md](docs/DEVELOPER_GUIDE.md)
- Сверка с ТЗ по разделам и известные ограничения: [docs/SPEC_COMPLIANCE.md](docs/SPEC_COMPLIANCE.md)
- Конкурентность, воркер, паники, таймауты и отказы подробно, с вопросами и ответами:
  [docs/CONCURRENCY.md](docs/CONCURRENCY.md)
- ТЗ: [29.md](29.md)

## 1. Запуск

Нужны Go 1.26+ и Docker.

```bash
docker compose up -d                 # PostgreSQL 16 на :5432, БД tickets
go run ./cmd/tickets                 # API на :8080, Swagger UI: http://localhost:8080/docs
```

При старте применяются миграции и, если включён `SEED` (по умолчанию да), создаются демо-данные:
- организатор `concert-company` с балансом 50 000;
- мероприятие `event-1`, места A1..A100 (A1–A80 по 2000, A81–A100 по 2500);
- служебные аккаунты `admin` / `admin-dev-password` и `provider` / `provider-dev-password`.

Для демонстрации TTL и проверки двух экземпляров:

```bash
export JWT_SECRET=$(openssl rand -hex 32)       # общий секрет: токен одного экземпляра принимает другой
RESERVATION_TTL=5s PAYMENT_TIMEOUT=10s WORKER_INTERVAL=1s LISTEN_ADDR=:8081 go run ./cmd/tickets
RESERVATION_TTL=5s PAYMENT_TIMEOUT=10s WORKER_INTERVAL=1s LISTEN_ADDR=:8082 go run ./cmd/tickets   # второй терминал
./scripts/demo.sh :8081 :8082                   # проходит сценарии раздела 55 ТЗ
```

> Если в БД уже есть таблицы сервиса, но нет `schema_migrations` (схема создана не миграциями), приложение
> остановится с понятным сообщением. Пересоздайте БД (`docker compose down -v && docker compose up -d`) или
> укажите пустую в `DATABASE_URL`.

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/tickets?sslmode=disable` | PostgreSQL |
| `LISTEN_ADDR` | `:8080` | адрес HTTP |
| `RESERVATION_TTL` | `5m` | время жизни брони |
| `PAYMENT_TIMEOUT` | `10m` | время на оплату после начала покупки |
| `WORKER_INTERVAL` | `5s` | период фонового воркера |
| `REQUEST_TIMEOUT` | `10s` | предел времени одного запроса (в основном ожидания блокировок); по истечении транзакция откатывается, ответ 503 `timeout` |
| `SHUTDOWN_TIMEOUT` | `15s` | сколько ждать активные запросы при остановке |
| `DB_MAX_CONNS` | по умолчанию pgxpool | размер пула соединений |
| `JWT_SECRET` | случайный при старте | ключ подписи HS256, ≥ 32 байт. **Задайте явно**, иначе токены не переживут рестарт и не будут приниматься другим экземпляром |
| `JWT_ISSUER` | `last-ticket` | claim `iss` |
| `ACCESS_TOKEN_TTL` | `15m` | время жизни access-токена |
| `REFRESH_TOKEN_TTL` | `168h` | время жизни refresh-токена |
| `BCRYPT_COST` | `10` | стоимость хеширования паролей |
| `ADMIN_LOGIN` / `ADMIN_PASSWORD` | `admin` / `admin-dev-password` при `SEED=true` | аккаунт администратора |
| `PROVIDER_LOGIN` / `PROVIDER_PASSWORD` | `provider` / `provider-dev-password` при `SEED=true` | аккаунт платёжной системы |
| `SEED` | `true` | демо-каталог и dev-пароли служебных аккаунтов |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Ошибки конфигурации выводятся все сразу, и приложение не стартует.

### Тесты

```bash
go test ./...          # PostgreSQL должен быть запущен
go test -race ./...
REQUIRE_DB=1 go test -race ./...   # для CI: без БД тесты падают, а не пропускаются
```

Каждый тестовый пакет сам создаёт и мигрирует свою БД (`tickets_test_booking`, `tickets_test_httpapi`, …).
Демо-данные не затрагиваются, пакеты не мешают друг другу. Базовый адрес задаётся через `TEST_DATABASE_URL`.
Если PostgreSQL недоступен, интеграционные тесты помечаются SKIP (видно с `-v`), юнит-тесты выполняются.

## 2. Архитектура

```
              Load Balancer
               /         \
     Go instance 1    Go instance 2      stateless: всё состояние в БД, время из часов БД
               \         /
               PostgreSQL                 блокировки строк, ограничения, история переходов
```

```
cmd/tickets          точка входа: конфиг, сигналы, запуск
internal/config      переменные окружения → Config (с валидацией)
internal/app         сборка зависимостей, сид, HTTP-сервер, воркер, graceful shutdown
internal/httpapi     маршруты и права доступа, middleware, хендлеры, ошибки → HTTP, OpenAPI
internal/auth        аккаунты, пароли (bcrypt), JWT, refresh-токены
internal/booking     бизнес-логика: бронь, покупка, оплата; SQL — в store*.go
internal/worker      периодический проход: истечение броней и таймаут платежей
internal/domain      сущности, статусы и state machine, ошибки (без зависимостей)
internal/postgres    пул, транзакции, миграции, классификация ошибок БД
internal/testutil    тестовая БД
scripts/demo.sh      сценарий демонстрации
```

Зависимости направлены внутрь: `httpapi → booking, auth → postgres, domain`. `domain` ни от чего не зависит.
Внешние библиотеки: `pgx` (драйвер PostgreSQL), `golang-jwt/jwt` (подпись и проверка JWT),
`x/crypto/bcrypt` (хеш паролей). HTTP, JSON и логи — стандартная библиотека.

## 3. Структура БД

Миграции: `internal/postgres/migrations/NNNN_*.sql`. Применённые версии записываются в `schema_migrations`.

```
organizers(id, name, balance ≥ 0, created_at)
users(id, name, login [уникален без учёта регистра], password_hash, role, balance ≥ 0, created_at)
refresh_tokens(id, user_id → users, token_hash UNIQUE, created_at, expires_at, revoked_at, replaced_by → refresh_tokens)
events(id, name, organizer_id → organizers, created_at)
seats(event_id → events, number, seat_order, price > 0, status)                  PK (event_id, number)
reservations(id, user_id → users, (event_id, seat_number) → seats, status, created_at, expires_at, finished_at)
purchases(id, reservation_id UNIQUE → reservations, status, started_at, finished_at)
payments(id, purchase_id UNIQUE → purchases, user_id, organizer_id, amount > 0, status, failure_reason,
         created_at, expires_at, completed_at)
transitions(id, entity, entity_id, from_status, to_status, reason, at)          история
```

Ограничения как последний барьер:
- `CHECK (balance >= 0)` у `users` и `organizers`: баланс физически не уходит в минус;
- частичный уникальный индекс `reservations_one_live_per_seat` на `(event_id, seat_number)
  WHERE status IN ('reserved','payment_pending','purchased')`: вторую «живую» бронь места БД не примет;
- `CHECK` на все статусы, уникальность `purchases.reservation_id` и `payments.purchase_id`.

## 4. Сущности и связи

`Organizer → Event → Seat → Reservation → Purchase → Payment`. `User → Reservation` (владелец),
`User → Payment` (плательщик), `Organizer → Payment` (получатель). Место идентифицируется парой
(`eventId`, `seatId`), где `seatId` — номер места (`A17`), как в примерах ТЗ. У одной брони не больше одной покупки,
у одной покупки ровно один платёж. Цена хранится только в `seats.price`. Сумма платежа берётся оттуда,
клиентское поле `price` отклоняется с 400.

## 5–6. Модель состояний и допустимые переходы

Таблицы переходов в `internal/domain/status.go` — единственный источник правды. Каждая смена статуса
в БД выполняется как `UPDATE … WHERE status = <from>`, а таблица переходов проверяется заранее. Поэтому выйти из
финального состояния нельзя даже при ошибке в коде.

```
Место:        available → reserved → payment_pending → purchased (финал)
                  ↑          │              │
                  └──────────┴──────────────┘   cancel / expire / fail / timeout

Бронь:        reserved ─ cancel ──────────────────────→ canceled          (финал)
                 │ ──── expire (TTL) ─────────────────→ expired           (финал)
                 └─ purchase → payment_pending ─┬─ confirm ─────────→ purchased      (финал)
                                                ├─ fail / нет денег ─→ payment_failed (финал)
                                                └─ payment timeout ──→ expired        (финал)

Покупка:      payment_pending → purchased | failed | timed_out   (все три финальные)
Платёж:       pending         → succeeded | failed | timed_out   (все три финальные)
```

Сверх минимума ТЗ у брони добавлены `purchased` (чтобы отличать «нельзя отменить проданное») и
`payment_failed`. Место освобождается после cancel, expire, fail (в том числе `insufficient_funds`) и timeout.
После `purchased` оно занято навсегда.

## Авторизация

| Роль | Кто | Что может |
|---|---|---|
| `user` | покупатель (самостоятельная регистрация) | бронировать, отменять и покупать свои брони, смотреть свои брони, покупки и платежи |
| `admin` | администратор | создавать организаторов и мероприятия, читать всё, вручную вызывать платёжные колбэки |
| `provider` | платёжная система | `POST /payments/{id}/confirm` и `/fail`, чтение платежей |

- `POST /users` регистрирует покупателя (`name`, `login`, `password`, `balance`). `POST /auth/login` возвращает
  `accessToken` (JWT HS256, по умолчанию 15 минут) и `refreshToken` (случайная строка, больше 128 бит, по
  умолчанию 7 дней). Дальше в запросах передаётся `Authorization: Bearer <accessToken>`. В Swagger UI для этого
  есть кнопка Authorize.
- Access-токен проверяется по подписи без обращения к БД: алгоритм только HS256 (`none` и подмена алгоритма
  отклоняются), плюс `iss`, `aud`, `exp`. Все экземпляры используют общий `JWT_SECRET`.
- `POST /auth/refresh` выдаёт новую пару, а старый refresh-токен отзывается (ротация). Если отозванный токен
  предъявлен повторно, значит его украли: отзываются все сессии пользователя.
- `POST /auth/logout` отзывает refresh-токен. Access-токен живёт до истечения (15 минут): это компромисс
  stateless-JWT.
- В БД хранятся только bcrypt-хеши паролей и SHA-256-хеши refresh-токенов. Неверный логин и неверный пароль
  отвечают одинаково и примерно за одинаковое время.
- Логины служебных аккаунтов не может занять посторонний: если `ADMIN_LOGIN` или `PROVIDER_LOGIN` уже
  принадлежит аккаунту с другой ролью, приложение не стартует.
- Покупатель всегда берётся из токена. Поле `userId` в теле запроса (как в ТЗ) необязательно, а если оно указано
  и не совпадает с вызывающим, ответ 403.
- Покупатель не может сам подтвердить свою оплату: `confirm` моделирует колбэк платёжной системы и доступен
  только ролям `provider` и `admin`.

## 7. Reservation TTL

`expires_at = now + RESERVATION_TTL`, время берётся из часов БД. Пока бронь `reserved`, место занято.
После `expires_at` бронь недействительна сразу, а не после прохода воркера:
- `Reserve` того же места под блокировкой переводит просроченную бронь в `expired` и занимает место;
- `purchase` и `cancel` просроченной брони отвечают 409 `reservation_expired` и сохраняют `expired`;
- `GET` показывает эффективный статус: бронь `expired`, место `available`.

## 8. Payment timeout

`purchase` переводит бронь в `payment_pending`, и с этого момента TTL брони не действует: воркер выбирает только
`status = 'reserved'`. Действует `payments.expires_at = now + PAYMENT_TIMEOUT`. После него платёж
`timed_out`, покупка `timed_out`, бронь `expired`, место `available`. Деньги не двигаются.

## 9. Модель тестовых балансов

Целые числа (без копеек) в `users.balance` и `organizers.balance`, от 0 до 10¹². Баланс покупателя задаётся
при регистрации, организатора — при создании. Реальных платёжных систем нет.

## 10. Механизм перевода денег

Деньги двигаются только при переходе платежа `pending → succeeded`, внутри `ConfirmPayment`, в одной транзакции:

```
BEGIN
  SELECT … FROM seats    WHERE … FOR UPDATE          -- место (общая точка сериализации)
  SELECT … FROM payments WHERE id = … FOR UPDATE     -- статус и дедлайн (clock_timestamp)
  SELECT balance FROM users WHERE id = … FOR UPDATE  -- баланс читается под блокировкой
  UPDATE users      SET balance = balance - amount WHERE id = … AND balance >= amount
  UPDATE organizers SET balance = balance + amount WHERE id = …
  UPDATE payments / purchases / reservations / seats SET status = … WHERE status = <from>
  INSERT INTO transitions …
COMMIT
```

Любая ошибка, включая отмену запроса клиентом, откатывает всё. Сценарий «списали, но не зачислили» невозможен:
либо выполнено всё, либо ничего. fail, timeout, опоздавший и повторный confirm деньги не трогают.

## 11. Границы транзакций

| Операция | Одна транзакция |
|---|---|
| Reserve | проверка покупателя, блокировка места, ленивое освобождение просроченной брони или платежа, INSERT брони, место → `reserved`, история |
| Cancel / Expire | блокировка места и брони, проверка владельца, статуса и TTL, бронь → `canceled`/`expired`, место → `available` |
| StartPurchase | блокировка места и брони, проверка владельца, статуса и TTL, INSERT покупки и платежа, бронь и место → `payment_pending` |
| Confirm | см. п. 10 |
| Fail / Timeout | блокировка места и платежа, платёж, покупка, бронь, место → финальные статусы |
| Refresh | блокировка refresh-токена, выпуск нового, отзыв старого (или отзыв всех сессий при повторном использовании) |

Отказы, которые нужно сохранить (например `insufficient_funds` или ленивое `expired`), коммитятся, а ошибка
возвращается вызывающему после коммита (`commitThen` в `booking/service.go`).

## 12. Защита от двойной брони

Механизм — пессимистическая блокировка строки места. Любая операция над местом начинается с
`SELECT … FROM seats WHERE event_id = ? AND number = ? FOR UPDATE`. Решение «последнее место теперь принадлежит
этому пользователю» принимается внутри транзакции `Reserve`: под блокировкой читается статус, вставляется бронь
и место переводится в `reserved`. Конкуренты ждут на блокировке, а получив её, видят `reserved` и отвечают 409.
Блокировку держит PostgreSQL, а не процесс. Поэтому два экземпляра приложения сериализуются так же, как две
горутины одного, и продать место дважды не могут.

Наивный `SELECT → INSERT` без блокировки не годится: оба читателя увидят `available`. Оптимистическая блокировка
при 100 соперниках дала бы 99 повторов, очередь на блокировке проще. Второй барьер — частичный уникальный индекс:
если он сработает, ответ тоже 409.

## 13. Защита от double spending

Строка покупателя блокируется (`SELECT balance … FOR UPDATE`), баланс проверяется только после этого.
Два конкурентных confirm разных платежей одного пользователя выстраиваются в очередь на этой строке, и второй
видит уже уменьшенный баланс (`insufficient_funds`). Дополнительно страхуют `UPDATE … WHERE balance >= amount`
и `CHECK (balance >= 0)`.

Порядок блокировок везде один: место → бронь/платёж → покупатель → организатор. Циклов ожидания нет, а значит
нет и дедлоков. Если временный сбой всё же произойдёт (deadlock, нехватка соединений), клиент получит
503 с `Retry-After`, транзакция при этом откатывается.

## 14. Идемпотентность confirm

Под блокировкой читается статус платежа. Для `succeeded` возвращается тот же ответ (200), деньги не двигаются,
вторая покупка не создаётся. Для `failed` ответ 409 `payment_not_pending`, для `timed_out` — 409
`payment_timed_out`. Параллельные дубликаты сериализуются блокировкой места: переводит первый, остальные видят
`succeeded`. `fail` идемпотентен так же.

## 15. Задержанное подтверждение

Бронь в 12:00, TTL до 12:05, покупка в 12:04, оплата до 12:14, confirm в 12:10. Подтверждение успешно, потому что
после `payment_pending` смотрится только дедлайн платежа (`TestDelayedConfirm`, `scripts/demo.sh`).

## 16. Слишком позднее подтверждение

Под блокировкой проверяется `payments.expires_at <= clock_timestamp()`. Если воркер ещё не отработал, confirm
сам переводит платёж в `timed_out`, освобождает место, коммитит это и отвечает 409 `payment_timed_out`.
Если место уже занял другой пользователь, ему ничего не грозит: старый платёж в финальном состоянии.
`timed_out → purchased` запрещён state machine. Деньги не двигаются (`TestLateConfirm`,
`TestLateConfirmWithoutSweeper`).

## 17–18. Воркер: истечение броней и таймаут платежей

`internal/worker.Run` при старте и затем каждые `WORKER_INTERVAL` делает два прохода:
- брони `status = 'reserved' AND expires_at <= now` → `expired`, место свободно;
- платежи `status = 'pending' AND expires_at <= now` → `timed_out` (с покупкой, бронью и местом).

Каждая запись обрабатывается в своей транзакции под блокировкой места, с перепроверкой условия. Поэтому
воркеры нескольких экземпляров и пользовательские операции не мешают друг другу: повторная обработка
ничего не делает. Ошибка одной записи не останавливает остальные. На одну запись отводится не больше
`REQUEST_TIMEOUT`: место, которое кто-то долго держит заблокированным, задерживает только свою запись. Паника
внутри прохода перехватывается и пишется в лог, процесс продолжает работать. Воркер только наводит порядок,
корректность от него не зависит (пп. 7, 8, 16). Подробнее — [docs/CONCURRENCY.md](docs/CONCURRENCY.md).

## 19. Поведение после restart

Всё состояние хранится в PostgreSQL. После рестарта просроченные брони и платежи не считаются активными, даже
если воркер ещё не прошёл: чтение показывает эффективный статус, а запись переводит их лениво (`TestRestart`).
Refresh-токены переживают рестарт. Access-токены тоже, если `JWT_SECRET` задан явно.

## 20. Результаты конкурентных тестов

`go test -race ./...` на PostgreSQL 16: все 8 пакетов проходят. Три прогона подряд без единого сбоя.

| Тест | Результат |
|---|---|
| Эксперимент 1: 100 параллельных reserve одного места | 1 успех, 99 конфликтов, в БД одна действующая бронь |
| То же через HTTP с JWT (100 пользователей) | 1×201, 99×409 |
| То же на двух живых экземплярах (`scripts/demo.sh :8081 :8082`) | 1×201, 99×409 |
| 60 reserve через два пула («два экземпляра») | 1 успех |
| Эксперимент 2: баланс 2000, два confirm по 2000 (20 повторов) | всегда 1 `succeeded` + 1 `insufficient_funds`; покупатель 0, организатор +2000 |
| 30 confirm по 100 при балансе 1000 | ровно 10 успешных, баланс 0 |
| Эксперимент 3: expire vs purchase на границе TTL (60 повторов) | всегда одно состояние: 49× `expired`, 11× `payment_pending` |
| Эксперимент 4: timeout vs confirm на дедлайне (60 повторов) | 50× `timed_out` (деньги на месте), 10× `purchased` (переведены); ни разу `timed_out` + списание |
| cancel vs purchase (40 повторов) | всегда ровно `canceled` или `payment_pending` |
| 20 параллельных дубликатов confirm | деньги переведены один раз, одна запись `succeeded` в истории |
| Два параллельных refresh одним токеном | ровно один успешен |

Соотношение исходов в гонках зависит от тайминга и меняется от запуска к запуску. Важно другое: недопустимого
состояния не было ни разу.

### Hotspots и contention (§47)

- **1000 клиентов на одно место.** Все ждут `FOR UPDATE` строки `seats`. Транзакция победителя короткая
  (проверка, вставка), остальные после неё сразу видят `reserved` и откатываются с 409. Конфликтов записи и
  повторов нет.
- **100 confirm на один баланс.** Все ждут блокировки строки `users`. Каждый видит актуальный баланс: первые
  `balance / price` успешны, остальные получают `insufficient_funds`.
- **Строка организатора** — общий hotspot всех продаж мероприятия. Она блокируется последней и ненадолго.
  При больших нагрузках её заменяют append-only журналом, но это за рамками задания.
- Ожидающая транзакция занимает соединение из пула, поэтому потолок одновременно ждущих — `DB_MAX_CONNS`.
  Ожидание ограничено `REQUEST_TIMEOUT`: если блокировку кто-то держит слишком долго, запрос откатывается с
  503 `timeout`, а не копится. При нехватке соединений на стороне PostgreSQL ответ тоже 503.

## 21. Graceful shutdown

По `SIGINT` или `SIGTERM` (`app.Serve`):
1. останавливается воркер: отменяется его контекст, текущая транзакция откатывается (или доводится до COMMIT,
   если работа уже сделана);
2. сервер перестаёт принимать соединения (`http.Server.Shutdown`);
3. активные запросы доделываются в пределах `SHUTDOWN_TIMEOUT`;
4. если время вышло, контекст оставшихся запросов отменяется, их транзакции откатываются, соединения
   закрываются (клиент получает обрыв);
5. `app.Serve` дожидается выхода воркера, после чего `main` закрывает пул соединений;
6. процесс завершается.

В ТЗ «перестать принимать запросы» стоит первым, а здесь воркер останавливается на мгновение раньше:
`Shutdown` не возвращается, пока не закончатся запросы, и остановка воркера после него оставила бы его работать
всё это время. Оба шага выполняются сразу, один за другим.

Проверено тестами `TestShutdownLetsActiveRequestsFinish` (запрос ждёт блокировку, приходит сигнал, запрос
успешно завершается) и `TestShutdownTimeoutCancelsRequests` (запрос не успевает, отменяется, бронь не создана).

## API

Swagger UI — `/docs`, спецификация — `/openapi.json` (тест сверяет её с таблицей маршрутов).

| Метод и путь | Доступ |
|---|---|
| `POST /users`, `POST /auth/login`, `POST /auth/refresh`, `POST /auth/logout` | публично |
| `GET /events`, `GET /events/{id}/seats`, `GET /healthz`, `GET /readyz` | публично |
| `GET /users/me`, `GET /users/{id}` | свой аккаунт или admin |
| `POST /organizers`, `GET /organizers/{id}`, `POST /events` | admin |
| `POST /reservations`, `POST /reservations/{id}/cancel`, `POST /reservations/{id}/purchase` | user (владелец) |
| `GET /reservations/{id}`, `GET /reservations/{id}/history`, `GET /purchases/{id}` | владелец или admin |
| `GET /payments/{id}` | владелец, admin или provider |
| `POST /payments/{id}/confirm`, `POST /payments/{id}/fail` | provider или admin |

Ошибки возвращаются как `{"error": "<код>", "message": "…"}`: 400 `validation_failed`,
401 `unauthenticated` / `invalid_token` / `invalid_credentials`, 403 `forbidden`, 404 `not_found`,
409 `seat_unavailable` / `invalid_transition` / `reservation_expired` / `payment_timed_out` /
`payment_not_pending` / `already_exists`, 402 `insufficient_funds` (в теле — сохранённый `payment`),
413 `body_too_large` (тело больше 1 МиБ), 503 `service_unavailable` / `timeout` / `request_cancelled`.
Каждый ответ содержит `X-Request-ID` для поиска в логах.

## Ответы на вопросы защиты

- **Где принимается решение, что последнее место принадлежит пользователю?** В транзакции `Reserve`, под
  `FOR UPDATE` строки места.
- **Почему два экземпляра не продадут место дважды?** Блокировка и уникальный индекс находятся в PostgreSQL,
  а не в процессе.
- **Почему два платежа не потратят баланс дважды?** Блокировка строки покупателя, проверка баланса после неё,
  `CHECK (balance >= 0)`.
- **Что если confirm пришёл одновременно с timeout?** Оба ждут блокировку места, побеждает первый. Если первым
  был timeout, confirm видит `timed_out` и получает 409, деньги целы. Если confirm, воркер видит `succeeded` и
  ничего не делает.
- **Почему повторный confirm не переводит деньги снова?** Он видит `succeeded` и возвращает результат.
- **Что если деньги списались, а начисление организатору упало?** Это одна транзакция: ошибка откатывает и
  списание. Либо произошло всё, либо ничего.
- **Может ли покупатель сам подтвердить свою оплату?** Нет, `confirm` доступен только ролям `provider` и `admin`.
