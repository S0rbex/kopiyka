-- One database and one user per service: a service can only reach its own data.
-- Local development credentials only.

CREATE USER merchant WITH PASSWORD 'merchant';
CREATE DATABASE merchant_db OWNER merchant;

CREATE USER payment WITH PASSWORD 'payment';
CREATE DATABASE payment_db OWNER payment;

CREATE USER ledger WITH PASSWORD 'ledger';
CREATE DATABASE ledger_db OWNER ledger;

-- By default any user may connect to any database; allow only the owner
REVOKE CONNECT ON DATABASE merchant_db, payment_db, ledger_db FROM PUBLIC;
