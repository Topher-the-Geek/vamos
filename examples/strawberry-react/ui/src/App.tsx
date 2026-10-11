import './App.css';
import reactLogo from './assets/react.svg';
import viteLogo from '/vite.svg';

import { useMutation, useQuery } from '@apollo/client/react';
import { GET_COUNTER, INCREMENT_COUNTER } from './queries';

function Counter() {
  const { loading, error, data } = useQuery(GET_COUNTER);
  const [increment] = useMutation(INCREMENT_COUNTER);

  if (loading)
    return <p>Loading...</p>;
  if (error)
    return <p>Error: {error.message}</p>;
  return (
    <button onClick={() => void increment()}>
      count is {data?.counter.value}
    </button>
  );
}

function App() {
  return (
    <>
      <div>
        <a href="https://vite.dev" target="_blank">
          <img src={viteLogo} className="logo" alt="Vite logo" />
        </a>
        <a href="https://react.dev" target="_blank">
          <img src={reactLogo} className="logo react" alt="React logo" />
        </a>
      </div>
      <h1>Vite + React</h1>
      <div className="card">
        <Counter />
        <p>
          Edit <code>src/App.tsx</code> and save to test HMR
        </p>
      </div>
      <p className="read-the-docs">
        Click on the Vite and React logos to learn more
      </p>
    </>
  )
}

export default App
