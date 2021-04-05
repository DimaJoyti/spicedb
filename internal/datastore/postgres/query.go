package postgres

import (
	"errors"
	"fmt"
	"runtime"

	sq "github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
	"github.com/authzed/spicedb/internal/datastore"
	pb "github.com/authzed/spicedb/pkg/REDACTEDapi/api"
)

const errUnableToQueryTuples = "unable to query tuples: %w"

var (
	errClosedIterator = errors.New("unable to iterate: iterator closed")

	queryTuples = psql.Select(
		colNamespace,
		colObjectID,
		colRelation,
		colUsersetNamespace,
		colUsersetObjectID,
		colUsersetRelation,
	).From(tableTuple)
)

func (pgd *pgDatastore) QueryTuples(namespace string, revision uint64) datastore.TupleQuery {
	return pgTupleQuery{
		db: pgd.db,
		query: queryTuples.
			Where(sq.Eq{colNamespace: namespace}).
			Where(sq.LtOrEq{colCreatedTxn: revision}).
			Where(sq.Or{
				sq.Eq{colDeletedTxn: liveDeletedTxnID},
				sq.Gt{colDeletedTxn: revision},
			}),
		namespace: namespace,
	}
}

type pgTupleQuery struct {
	db        *sqlx.DB
	query     sq.SelectBuilder
	namespace string
	relation  string
}

func (ptq pgTupleQuery) WithObjectID(objectID string) datastore.TupleQuery {
	ptq.query = ptq.query.Where(sq.Eq{colObjectID: objectID})
	return ptq
}

func (ptq pgTupleQuery) WithRelation(relation string) datastore.TupleQuery {
	ptq.query = ptq.query.Where(sq.Eq{colRelation: relation})
	ptq.relation = relation
	return ptq
}

func (ptq pgTupleQuery) WithUserset(userset *pb.ObjectAndRelation) datastore.TupleQuery {
	ptq.query = ptq.query.Where(sq.Eq{
		colUsersetNamespace: userset.Namespace,
		colUsersetObjectID:  userset.ObjectId,
		colUsersetRelation:  userset.Relation,
	})
	return ptq
}

func (ptq pgTupleQuery) Execute() (datastore.TupleIterator, error) {
	tx, err := ptq.db.Beginx()
	if err != nil {
		return nil, fmt.Errorf(errUnableToQueryTuples, err)
	}
	defer tx.Rollback()

	if err != nil {
		return nil, err
	}

	sql, args, err := ptq.query.ToSql()
	if err != nil {
		return nil, fmt.Errorf(errUnableToQueryTuples, err)
	}

	rows, err := ptq.db.Queryx(sql, args...)
	if err != nil {
		return nil, fmt.Errorf(errUnableToQueryTuples, err)
	}

	var tuples []*pb.RelationTuple
	for rows.Next() {
		nextTuple := &pb.RelationTuple{
			ObjectAndRelation: &pb.ObjectAndRelation{},
			User: &pb.User{
				UserOneof: &pb.User_Userset{
					Userset: &pb.ObjectAndRelation{},
				},
			},
		}
		userset := nextTuple.User.GetUserset()
		err := rows.Scan(
			&nextTuple.ObjectAndRelation.Namespace,
			&nextTuple.ObjectAndRelation.ObjectId,
			&nextTuple.ObjectAndRelation.Relation,
			&userset.Namespace,
			&userset.ObjectId,
			&userset.Relation,
		)
		if err != nil {
			return nil, fmt.Errorf(errUnableToQueryTuples, err)
		}

		tuples = append(tuples, nextTuple)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(errUnableToQueryTuples, err)
	}

	iter := &pgTupleIterator{
		tuples: tuples,
	}

	runtime.SetFinalizer(iter, func(iter *pgTupleIterator) {
		if !iter.closed {
			panic(fmt.Sprintf(
				"Tuple iterator garbage collected before Close() was called\n sql: %s\n args: %#v\n",
				sql,
				args,
			))
		}
	})

	return iter, nil
}

type pgTupleIterator struct {
	tuples []*pb.RelationTuple
	closed bool
	err    error
}

func (pti *pgTupleIterator) Next() *pb.RelationTuple {
	if pti.closed {
		pti.err = errClosedIterator
		return nil
	}

	if len(pti.tuples) > 0 {
		first := pti.tuples[0]
		pti.tuples = pti.tuples[1:]
		return first
	}

	return nil
}

func (pti *pgTupleIterator) Err() error {
	return pti.err
}

func (pti *pgTupleIterator) Close() {
	if pti.closed {
		panic("postgres iterator double closed")
	}

	pti.tuples = nil
	pti.closed = true
}