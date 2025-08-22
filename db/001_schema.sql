-- Enable TimescaleDB extension
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Raw metrics table
CREATE TABLE IF NOT EXISTS metrics (
    time        TIMESTAMPTZ NOT NULL,
    name        TEXT NOT NULL,
    value       DOUBLE PRECISION,
    labels      JSONB DEFAULT '{}'::jsonb
);

SELECT create_hypertable('metrics', 'time', if_not_exists => TRUE);

-- 1 minute continuous aggregate
CREATE MATERIALIZED VIEW IF NOT EXISTS metrics_1m
WITH (timescaledb.continuous) AS
SELECT time_bucket(INTERVAL '1 minute', time) AS bucket,
       name,
       avg(value) AS avg_value
FROM metrics
GROUP BY bucket, name;

-- TopK view
CREATE OR REPLACE VIEW topk_metrics AS
SELECT bucket, name, avg_value
FROM (
    SELECT bucket, name, avg_value,
           RANK() OVER (PARTITION BY bucket ORDER BY avg_value DESC) AS r
    FROM metrics_1m
) ranked
WHERE r <= 5;

-- Incident and audit tables
CREATE TABLE IF NOT EXISTS incidents (
    id SERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ DEFAULT now(),
    description TEXT,
    status TEXT DEFAULT 'open'
);

CREATE TABLE IF NOT EXISTS audits (
    id SERIAL PRIMARY KEY,
    incident_id INT REFERENCES incidents(id),
    checked_at TIMESTAMPTZ DEFAULT now(),
    details TEXT
);

-- Demo function
CREATE OR REPLACE FUNCTION demo_generate_metrics() RETURNS VOID AS $$
BEGIN
    INSERT INTO metrics (time, name, value)
    VALUES (now(), 'cpu', random()*100);
END;
$$ LANGUAGE plpgsql;

-- Seed latency samples for demos
CREATE OR REPLACE FUNCTION seed_latency(svc TEXT, base INT, count INT) RETURNS VOID AS $$
DECLARE
    i INT;
BEGIN
    FOR i IN 1..count LOOP
        INSERT INTO metrics(time, name, value)
        VALUES (now() - (count - i || ' seconds')::interval, svc, base + random()*50);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- Return true if last 5 min average improved >=10% vs previous window
CREATE OR REPLACE FUNCTION recent_latency_improved(svc TEXT) RETURNS BOOLEAN AS $$
DECLARE
    prev_avg FLOAT;
    curr_avg FLOAT;
BEGIN
    SELECT COALESCE(avg(avg_value),0) INTO prev_avg
      FROM metrics_1m
     WHERE name=svc AND bucket > now() - interval '10 minutes'
       AND bucket <= now() - interval '5 minutes';

    SELECT COALESCE(avg(avg_value),0) INTO curr_avg
      FROM metrics_1m
     WHERE name=svc AND bucket > now() - interval '5 minutes';

    IF prev_avg = 0 THEN
        RETURN FALSE;
    END IF;
    RETURN (prev_avg - curr_avg) / prev_avg >= 0.1;
END;
$$ LANGUAGE plpgsql;
