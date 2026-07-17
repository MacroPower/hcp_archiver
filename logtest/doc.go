// Package logtest asserts over structured log events in tests.
//
// The archive's safety valves — a replay discarding records for a deleted
// subtree, a torn log truncated at its tear, a refused prune — deliberately
// trade completeness for safety and announce themselves as log events. A test
// exercising the machinery around a valve usually needs to prove the valve
// did NOT fire: a silently activated valve can absorb exactly the state the
// test asserts on, leaving every check to pass vacuously. [FailOn] builds a
// logger that fails the test the moment a named event fires, so a suite pins
// "no valve fired here" as an assertion instead of an assumption.
package logtest
