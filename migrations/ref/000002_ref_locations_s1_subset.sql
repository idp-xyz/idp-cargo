-- IDP-Cargo reference data / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The S1 location subset (decision F2, option 4; M1: development and internal acceptance on simulated and
-- subset data). Locations are copied verbatim (code, name, function classifiers, status) from the UNECE
-- UN/LOCODE release of 2026-11-01 as mirrored by github.com/datasets/un-locode at commit 0fb7229b6447,
-- data/code-list.csv, SHA-256 1dc3bf5baf762ae644ec4247fefcb58f92cb1720fa84170417d71598b77843f0.
-- The IATA airport codes are the assessor's S1 compilation, not IATA data: an airport-level entry gets its own
-- three letters, a city entry only when the city has one airport with that code; cities of several airports
-- (USNYC, JPTYO, USCHI, THBKK, CNSGH) get none and so are refused for AIR. The SEA aliases are codes carriers
-- have used for these ports before UNECE moved the port function to a new code. Both are to be verified
-- against IATA and carrier data, or replaced by the full import, before the pilot (E3).

INSERT INTO cargo_ref.location_edition (edition, source, source_digest, published, note, is_current) VALUES ('UNLOCODE-2026-11-01-S1',
  'UNECE UN/LOCODE release via github.com/datasets/un-locode 0fb7229b6447 data/code-list.csv', 'sha256:1dc3bf5baf762ae644ec4247fefcb58f92cb1720fa84170417d71598b77843f0', DATE '2026-11-01',
  'S1 subset for the M1 first lines; IATA codes and aliases compiled by the assessor, to be verified before E3', true);

INSERT INTO cargo_ref.location (edition, unlocode, name, country_code, functions, status) VALUES
  ('UNLOCODE-2026-11-01-S1', 'CNSGH', 'Shanghai', 'CN', '12345---', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNSHA', 'Shanghai Hongqiao International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNPVG', 'Shanghai Pudong International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNSHG', 'Shanghai Pt', 'CN', '1-------', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNNBO', 'Ningbo', 'CN', '12-4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNNBG', 'Ningbo Pt', 'CN', '1-------', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNNGB', 'Ningbo Lishe International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNSNZ', 'Shenzhen', 'CN', '12345---', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNSZX', 'Shenzhen Baoan International Apt', 'CN', '---45---', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNYTN', 'Yantian Pt', 'CN', '123--6--', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNSHK', 'Shekou Pt', 'CN', '1-------', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNCAN', 'Guangzhou Baiyun International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNQDG', 'Qingdao Pt', 'CN', '1-------', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNTAO', 'Qingdao Jiaodong International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNXMG', 'Xiamen Pt', 'CN', '1-------', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNXMN', 'Xiamen Gaoqi International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNTXG', 'Tianjin Xingang Pt', 'CN', '1-------', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'CNTSN', 'Tianjin Binhai International Apt', 'CN', '---4----', 'AS'),
  ('UNLOCODE-2026-11-01-S1', 'HKHKG', 'Hong Kong', 'HK', '1--45---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USLAX', 'Los Angeles', 'US', '1--45---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USLGB', 'Long Beach', 'US', '1234----', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USNYC', 'New York', 'US', '12345---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USJFK', 'John F. Kennedy Apt/New York', 'US', '---4----', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USEWR', 'Newark', 'US', '1234----', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USORD', 'O''Hare Apt/Chicago', 'US', '---45---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'USCHI', 'Chicago', 'US', '1--45---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'DEHAM', 'Hamburg', 'DE', '12345---', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'DEFRA', 'Frankfurt am Main', 'DE', '12345---', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'NLRTM', 'Rotterdam', 'NL', '12345---', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'NLAMS', 'Amsterdam', 'NL', '12345---', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'GBLHR', 'Heathrow Apt/London', 'GB', '---4----', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'GBFXT', 'Felixstowe', 'GB', '1-------', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'GBLON', 'London', 'GB', '1---5---', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'JPTYO', 'Tokyo', 'JP', '12345---', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'JPNRT', 'Narita Apt/Tokyo', 'JP', '---4----', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'JPHND', 'Haneda Apt/Tokyo', 'JP', '---4----', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'SGSIN', 'Singapore', 'SG', '1--45---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'AEDXB', 'Dubai', 'AE', '1-345--6', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'AEJEA', 'Jebel Ali', 'AE', '1-------', 'QQ'),
  ('UNLOCODE-2026-11-01-S1', 'KRICN', 'Incheon Intl Apt/Seoul', 'KR', '---4----', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'KRPUS', 'Busan', 'KR', '1234567-', 'AF'),
  ('UNLOCODE-2026-11-01-S1', 'THBKK', 'Bangkok', 'TH', '12345---', 'AI'),
  ('UNLOCODE-2026-11-01-S1', 'VNSGN', 'Ho Chi Minh City', 'VN', '1-3456--', 'AI');

INSERT INTO cargo_ref.airport_code (edition, iata, unlocode, source) VALUES
  ('UNLOCODE-2026-11-01-S1', 'AMS', 'NLAMS', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'CAN', 'CNCAN', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'DXB', 'AEDXB', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'EWR', 'USEWR', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'FRA', 'DEFRA', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'HKG', 'HKHKG', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'HND', 'JPHND', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'ICN', 'KRICN', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'JFK', 'USJFK', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'LAX', 'USLAX', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'LHR', 'GBLHR', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'NGB', 'CNNGB', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'NRT', 'JPNRT', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'ORD', 'USORD', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'PVG', 'CNPVG', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'SGN', 'VNSGN', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'SHA', 'CNSHA', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'SIN', 'SGSIN', 'S1 compilation: single-airport city entry'),
  ('UNLOCODE-2026-11-01-S1', 'SZX', 'CNSZX', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'TAO', 'CNTAO', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'TSN', 'CNTSN', 'S1 compilation: airport entry'),
  ('UNLOCODE-2026-11-01-S1', 'XMN', 'CNXMN', 'S1 compilation: airport entry');

INSERT INTO cargo_ref.location_alias (edition, mode, alias, unlocode, source) VALUES
  ('UNLOCODE-2026-11-01-S1', 'SEA', 'CNSHA', 'CNSHG', 'carrier convention for Shanghai Pt; CNSHA is now Shanghai Hongqiao International Apt'),
  ('UNLOCODE-2026-11-01-S1', 'SEA', 'CNNGB', 'CNNBG', 'carrier convention for Ningbo Pt; CNNGB is now Ningbo Lishe International Apt'),
  ('UNLOCODE-2026-11-01-S1', 'SEA', 'CNTAO', 'CNQDG', 'carrier convention for Qingdao Pt; CNTAO is now Qingdao Jiaodong International Apt'),
  ('UNLOCODE-2026-11-01-S1', 'SEA', 'CNXMN', 'CNXMG', 'carrier convention for Xiamen Pt; CNXMN is now Xiamen Gaoqi International Apt');
