-- ledger-service: schema inicial (ADR-0009).
-- Executado pelo papel dono (ledger_owner). O serviço roda como ledger_app, que
-- NÃO recebe UPDATE/DELETE em journal/entry nem UPDATE em saldo.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_app') THEN
        BEGIN
            CREATE ROLE ledger_app NOLOGIN;
        EXCEPTION WHEN duplicate_object OR unique_violation THEN
            NULL; -- criado concorrentemente (papel é global ao cluster)
        END;
    END IF;
END $$;

CREATE TABLE ledger_account (
    id           uuid PRIMARY KEY,
    external_ref text NOT NULL UNIQUE CHECK (length(external_ref) BETWEEN 1 AND 128),
    account_type text NOT NULL CHECK (account_type IN ('CUSTOMER','SETTLEMENT_TRANSIT','RESERVE','FEES','INTERNAL')),
    normal_side  text NOT NULL CHECK (normal_side IN ('DEBIT','CREDIT')),
    currency     char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    allow_negative boolean NOT NULL DEFAULT false,
    status       text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','FROZEN','CLOSED')),
    balance      bigint NOT NULL DEFAULT 0,
    version      bigint NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT balance_non_negative CHECK (balance >= 0 OR allow_negative),
    CONSTRAINT normal_side_matches_type CHECK (
        (account_type IN ('CUSTOMER','SETTLEMENT_TRANSIT','FEES') AND normal_side = 'CREDIT') OR
        (account_type IN ('RESERVE','INTERNAL') AND normal_side = 'DEBIT')),
    CONSTRAINT customer_never_negative CHECK (account_type <> 'CUSTOMER' OR NOT allow_negative)
);

CREATE TABLE journal (
    id           uuid PRIMARY KEY,
    kind         text NOT NULL CHECK (kind IN ('HOLD','SETTLEMENT','REVERSAL','INTERNAL_TRANSFER','EXTERNAL_CREDIT')),
    reference_id text NOT NULL CHECK (length(reference_id) BETWEEN 1 AND 128),
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (octet_length(metadata::text) <= 4096),
    tx_id        xid8 NOT NULL DEFAULT pg_current_xact_id(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (reference_id, kind)
);

CREATE TABLE entry (
    id            uuid PRIMARY KEY,
    seq           bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    journal_id    uuid NOT NULL REFERENCES journal(id),
    account_id    uuid NOT NULL REFERENCES ledger_account(id),
    direction     text NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount        bigint NOT NULL CHECK (amount > 0),
    currency      char(3) NOT NULL,
    balance_after bigint NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX entry_account_seq_idx ON entry (account_id, seq);
CREATE INDEX entry_journal_idx ON entry (journal_id);
CREATE INDEX entry_account_created_idx ON entry (account_id, created_at);

CREATE TABLE hold (
    id           uuid PRIMARY KEY,
    reference_id text NOT NULL UNIQUE,
    account_id   uuid NOT NULL REFERENCES ledger_account(id),
    journal_id   uuid REFERENCES journal(id),
    amount       bigint NOT NULL CHECK (amount > 0),
    currency     char(3) NOT NULL,
    status       text NOT NULL CHECK (status IN ('HELD','SETTLED','RELEASED','CANCELLED')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX hold_account_held_idx ON hold (account_id) WHERE status = 'HELD';
CREATE INDEX hold_open_age_idx ON hold (created_at) WHERE status = 'HELD';

CREATE TABLE command_log (
    reference_id text NOT NULL,
    command      text NOT NULL CHECK (command IN ('HOLD','SETTLE','RELEASE','TRANSFER','CREDIT')),
    request_hash text NOT NULL,
    outcome      text NOT NULL CHECK (outcome IN ('SUCCESS','FAILURE','NOOP')),
    result       jsonb NOT NULL,
    journal_id   uuid REFERENCES journal(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (reference_id, command)
);

CREATE TABLE balance_snapshot (
    account_id uuid NOT NULL REFERENCES ledger_account(id),
    day        date NOT NULL,
    balance    bigint NOT NULL,
    held       bigint NOT NULL,
    PRIMARY KEY (account_id, day)
);

CREATE TABLE outbox (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_id text NOT NULL,
    topic        text NOT NULL,
    msg_key      text NOT NULL,
    payload      bytea NOT NULL, -- bytes exatos: a assinatura HMAC cobre o corpo
    headers      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

CREATE TABLE inbox (
    message_id   text NOT NULL,
    consumer     text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, consumer)
);

-- Aplica o lançamento ao saldo: único caminho que altera ledger_account.balance.
CREATE FUNCTION ledger_apply_entry() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    acct   ledger_account%ROWTYPE;
    jtx    xid8;
    delta  bigint;
    newbal bigint;
BEGIN
    SELECT tx_id INTO jtx FROM journal WHERE id = NEW.journal_id;
    IF jtx IS NULL OR jtx <> pg_current_xact_id() THEN
        RAISE EXCEPTION 'entries may only be added in the transaction that created the journal'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    SELECT * INTO acct FROM ledger_account WHERE id = NEW.account_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown account %', NEW.account_id USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF acct.currency <> NEW.currency THEN
        RAISE EXCEPTION 'currency mismatch' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    delta := CASE WHEN NEW.direction = acct.normal_side THEN NEW.amount ELSE -NEW.amount END;
    newbal := acct.balance + delta;  -- overflow de bigint levanta exceção

    UPDATE ledger_account SET balance = newbal, version = version + 1 WHERE id = acct.id;
    NEW.balance_after := newbal;
    RETURN NEW;
END $$;

CREATE TRIGGER entry_apply BEFORE INSERT ON entry
    FOR EACH ROW EXECUTE FUNCTION ledger_apply_entry();

-- Partidas dobradas: verificada no fechamento da transação.
CREATE FUNCTION ledger_check_journal_balanced() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE
    d bigint; c bigint; n int; cur int;
BEGIN
    SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'DEBIT'), 0),
           COALESCE(SUM(amount) FILTER (WHERE direction = 'CREDIT'), 0),
           COUNT(*), COUNT(DISTINCT currency)
      INTO d, c, n, cur
      FROM entry WHERE journal_id = NEW.journal_id;
    IF n < 2 OR cur <> 1 OR d <> c THEN
        RAISE EXCEPTION 'journal % is not balanced (debits=%, credits=%, entries=%, currencies=%)',
            NEW.journal_id, d, c, n, cur USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER entry_balanced AFTER INSERT ON entry
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_journal_balanced();

-- Imutabilidade (vale inclusive para o dono).
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER journal_immutable BEFORE UPDATE OR DELETE ON journal FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER journal_no_truncate BEFORE TRUNCATE ON journal FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER entry_immutable BEFORE UPDATE OR DELETE ON entry FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER entry_no_truncate BEFORE TRUNCATE ON entry FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER command_log_immutable BEFORE UPDATE OR DELETE ON command_log FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER command_log_no_truncate BEFORE TRUNCATE ON command_log FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER account_no_delete BEFORE DELETE ON ledger_account FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER account_no_truncate BEFORE TRUNCATE ON ledger_account FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER hold_no_delete BEFORE DELETE ON hold FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER hold_no_truncate BEFORE TRUNCATE ON hold FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();

-- Conta: só status pode mudar; saldo só muda via ledger_apply_entry (trigger depth 2).
CREATE FUNCTION ledger_guard_account_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.external_ref <> OLD.external_ref OR NEW.account_type <> OLD.account_type
       OR NEW.normal_side <> OLD.normal_side OR NEW.currency <> OLD.currency
       OR NEW.allow_negative <> OLD.allow_negative OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'immutable account attribute' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF (NEW.balance <> OLD.balance OR NEW.version <> OLD.version) AND pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'balance can only change through entries' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.status = 'CLOSED' AND NEW.status <> 'CLOSED' THEN
        RAISE EXCEPTION 'closed account cannot be reopened' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER account_guard BEFORE UPDATE ON ledger_account
    FOR EACH ROW EXECUTE FUNCTION ledger_guard_account_update();

-- Hold: máquina de estados fechada.
CREATE FUNCTION ledger_guard_hold_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.reference_id <> OLD.reference_id OR NEW.account_id <> OLD.account_id
       OR NEW.amount <> OLD.amount OR NEW.currency <> OLD.currency
       OR NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'immutable hold attribute' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT (OLD.status = 'HELD' AND NEW.status IN ('SETTLED','RELEASED')) THEN
        RAISE EXCEPTION 'invalid hold transition % -> %', OLD.status, NEW.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER hold_guard BEFORE UPDATE ON hold
    FOR EACH ROW EXECUTE FUNCTION ledger_guard_hold_update();

-- Privilégios mínimos do papel de aplicação.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO ledger_app;
GRANT INSERT ON ledger_account, journal, entry, hold, command_log, balance_snapshot, outbox, inbox TO ledger_app;
GRANT UPDATE (status) ON ledger_account TO ledger_app;
GRANT UPDATE (status, updated_at) ON hold TO ledger_app;
GRANT UPDATE (published_at) ON outbox TO ledger_app;
GRANT DELETE ON outbox, inbox TO ledger_app;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC;
