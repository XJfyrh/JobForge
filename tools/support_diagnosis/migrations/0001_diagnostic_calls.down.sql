-- Only before any real request. Dropping these tables destroys billing evidence.
drop table diagnosis_calls;
drop table diagnosis_experiments;
drop table diagnosis_database_identity;
