//! codeintel-indexer-rs library surface.
//!
//! Phase R.1 shipped the standalone `clone` CLI; R.2 grows a
//! Redis worker loop. Both consume the same internal `clone`
//! function so the binary's `clone` subcommand and the worker
//! produce byte-equal output for the same inputs.

pub mod ast_extractor;
pub mod branches;
pub mod clone;
pub mod code_graph_model;
pub mod delta_plan;
pub mod executor_service;
pub mod fact_to_snapshot;
pub mod manifest_files;
pub mod manifest_store;
pub mod nebula_ngql;
pub mod proto;
pub mod queue;
pub mod scip;
pub mod worker;
pub mod zoekt;
