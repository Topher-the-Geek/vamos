import { gql, type TypedDocumentNode } from '@apollo/client';
import type {
  CounterQuery,
  CounterQueryVariables,
  IncrementMutation,
  IncrementMutationVariables,
} from './gql/types';

export const GET_COUNTER: TypedDocumentNode<CounterQuery, CounterQueryVariables> = gql`
  query counter {
    counter {
      id
      value
    }
  }
`;

export const INCREMENT_COUNTER: TypedDocumentNode<IncrementMutation, IncrementMutationVariables> = gql`
  mutation increment {
    increment {
      id
      value
    }
  }
`;
