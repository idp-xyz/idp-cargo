-- PLACEHOLDER of batch B's checkout only: the migrator numbers a context's migrations from 000001 without a gap, and
-- batch A (profiles, onboarding, the exception report) takes fin/000022 in the main checkout. The main session removes
-- this file when it joins the two and takes batch A's own fin/000022. It changes nothing.
SELECT 1;
