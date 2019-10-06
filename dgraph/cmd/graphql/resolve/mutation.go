/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package resolve

import (
	"bytes"
	"context"

	"github.com/dgraph-io/dgraph/dgraph/cmd/graphql/dgraph"
	"github.com/dgraph-io/dgraph/dgraph/cmd/graphql/schema"
	"github.com/dgraph-io/dgraph/x"
	"github.com/vektah/gqlparser/gqlerror"
	otrace "go.opencensus.io/trace"
)

// Mutations come in like this with variables:
//
// mutation themutation($post: PostInput!) {
//   addPost(input: $post) { ... some query ...}
// }
// - with variable payload
// { "post":
//   { "title": "My Post",
//     "author": { authorID: 0x123 },
//     ...
//   }
// }
//
//
// Or, like this with the payload in the mutation arguments
//
// mutation themutation {
//   addPost(input: { title: ... }) { ... some query ...}
// }
//
//
// Either way we build up a Dgraph json mutation to add the object
//
// For now, all mutations are only 1 level deep (cause of how we build the
// input objects) and only create a single node (again cause of inputs)

// mutationResolver can resolve a single GraphQL mutation field
type mutationResolver struct {
	mutation         schema.Mutation
	schema           schema.Schema
	mutationRewriter dgraph.MutationRewriter
	queryRewriter    dgraph.QueryRewriter
	dgraph           dgraph.Client
}

const (
	mutationFailed    = false
	mutationSucceeded = true
)

// resolve a single mutation, returning the result of resolving the mutation and
// a bool where true indicates that the mutation itself succeeded and false indicates
// that some error prevented the actual mutation.
func (mr *mutationResolver) resolve(ctx context.Context) (*resolved, bool) {
	// A mutation operation can contain any number of mutation fields.  Those should be executed
	// serially.
	// (spec https://graphql.github.io/graphql-spec/June2018/#sec-Normal-and-Serial-Execution)
	//
	// The spec is ambigous about what to do in the case of errors during that serial execution
	// - apparently deliberatly so; see this comment from Lee Byron:
	// https://github.com/graphql/graphql-spec/issues/277#issuecomment-385588590
	// and clarification
	// https://github.com/graphql/graphql-spec/pull/438
	//
	// A reasonable interpretation of that is to stop a list of mutations after the first error -
	// which seems like the natural semantics and is what we enforce here.
	//
	// What we aren't following the exact semantics for is the error propagation.
	// According to the spec
	// https://graphql.github.io/graphql-spec/June2018/#sec-Executing-Selection-Sets,
	// https://graphql.github.io/graphql-spec/June2018/#sec-Errors-and-Non-Nullability
	// and the commentry here:
	// https://github.com/graphql/graphql-spec/issues/277
	//
	// If we had a schema with:
	//
	// type Mutation {
	// 	 push(val: Int!): Int!
	// }
	//
	// and then ran operation:
	//
	//  mutation {
	// 	  one: push(val: 1)
	// 	  thirteen: push(val: 13)
	// 	  two: push(val: 2)
	//  }
	//
	// if `push(val: 13)` fails with an error, then only errors should be returned from the whole
	// mutation` - because the result value is ! and one of them failed, the error should propagate
	// to the entire operation. That is, even though `push(val: 1)` succeeded and we already
	// calculated its result value, we should squash that and return null data and an error.
	// (nothing in GraphQL says where any transaction or persistence boundries lie)
	//
	// We aren't doing that below - we aren't even inspecting if the result type is !.  For now,
	// we'll return any data we've already calculated and following errors.  However:
	// TODO: we should be picking through all results and propagating errors according to spec
	// TODO: and, we should have all mutation return types not have ! so we avoid the above

	var res *resolved
	var mutationSucceeded bool
	switch mr.mutation.MutationType() {
	case schema.AddMutation:
		res, mutationSucceeded = mr.resolveMutation(ctx)
	case schema.DeleteMutation:
		res, mutationSucceeded = mr.resolveDeleteMutation(ctx)
	case schema.UpdateMutation:
		// TODO: this should typecheck the input before resolving (like delete does)
		res, mutationSucceeded = mr.resolveMutation(ctx)
	default:
		return &resolved{
			err: gqlerror.Errorf(
				"Only add, delete and update mutations are implemented")}, mutationFailed
	}

	// Mutations have an extra element to their result path.  Because the mutation
	// always looks like `addBlaa(...) { blaa { ... } }` and what's resolved above
	// is the `blaa { ... }`, both the result and any error paths need a prefox
	// of `addBlaa`
	var b bytes.Buffer
	b.WriteRune('"')
	b.WriteString(mr.mutation.ResponseName())
	b.WriteString(`": `)
	if len(res.data) > 0 {
		b.Write(res.data)
	} else {
		b.WriteString("null")
	}
	res.data = b.Bytes()

	resErrs := schema.AsGQLErrors(res.err)
	var errs x.GqlErrorList = make([]*x.GqlError, len(resErrs))
	for i, err := range resErrs {
		if len(err.Path) > 0 {
			err.Path = append([]interface{}{mr.mutation.ResponseName()}, err.Path...)
		}
		errs[i] = err
	}
	res.err = errs

	return res, mutationSucceeded
}

func (mr *mutationResolver) resolveMutation(ctx context.Context) (*resolved, bool) {
	res := &resolved{}
	span := otrace.FromContext(ctx)
	stop := x.SpanTimer(span, "resolveMutation")
	defer stop()
	if span != nil {
		span.Annotatef(nil, "mutation alias: [%s] type: [%s]", mr.mutation.Alias(),
			mr.mutation.MutationType())
	}

	mut, err := mr.mutationRewriter.Rewrite(mr.mutation)
	if err != nil {
		res.err = schema.GQLWrapf(err, "couldn't rewrite mutation")
		return res, mutationFailed
	}

	assigned, err := mr.dgraph.Mutate(ctx, mut)
	if err != nil {
		res.err = schema.GQLWrapLocationf(err,
			mr.mutation.Location(),
			"mutation %s failed", mr.mutation.Name())
		return res, mutationFailed
	}

	dgQuery, err := mr.queryRewriter.FromMutationResult(mr.mutation, assigned)
	if err != nil {
		res.err = schema.GQLWrapf(err, "couldn't rewrite mutation %s",
			mr.mutation.Name())
		return res, mutationSucceeded
	}

	resp, err := mr.dgraph.Query(ctx, dgQuery)
	if err != nil {
		res.err = schema.GQLWrapf(err, "mutation %s created a node but query failed",
			mr.mutation.Name())
		return res, mutationSucceeded
	}

	res.data, res.err = completeDgraphResult(ctx, mr.mutation.QueryField(), resp)
	return res, mutationSucceeded
}

func (mr *mutationResolver) resolveDeleteMutation(ctx context.Context) (*resolved, bool) {
	res := &resolved{}
	span := otrace.FromContext(ctx)
	stop := x.SpanTimer(span, "resolveDeleteMutation")
	defer stop()
	if span != nil {
		span.Annotatef(nil, "mutation alias: [%s] type: [%s]", mr.mutation.Alias(),
			mr.mutation.MutationType())
	}

	query, mut, err := mr.mutationRewriter.RewriteDelete(mr.mutation)
	if err != nil {
		res.err = schema.GQLWrapf(err, "couldn't rewrite mutation")
		return res, mutationFailed
	}

	if err = mr.dgraph.DeleteNodes(ctx, query, mut); err != nil {
		res.err = schema.GQLWrapf(err, "mutation %s failed", mr.mutation.Name())
		return res, mutationFailed
	}

	res.data = []byte(`{ "msg": "Deleted" }`)
	return res, mutationSucceeded
}