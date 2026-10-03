/*
 * Copyright 2026 The Gremlins Authors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package schemata

// ConstMutation exposes constMutation to the external tests, which hold it
// against the engine's table.
var ConstMutation = constMutation

// RewriterFactory exposes rewriterFactory, for tests that force a rewrite.
type RewriterFactory = rewriterFactory

// RewritePackageWith exposes rewritePackage with an injected rewriter.
var RewritePackageWith = rewritePackage

// BinaryNames exposes binaryNames.
var BinaryNames = binaryNames

// AssignMutation exposes assignMutation to the external tests, which hold
// it against the engine's table.
var AssignMutation = assignMutation
