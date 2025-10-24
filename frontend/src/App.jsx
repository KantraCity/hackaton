import { useState } from 'react';
import { GenerateAndCreateFiles } from '../wailsjs/go/main/App';
import './App.css';

function App() {
    const [clientQuery, setClientQuery] = useState('Лоток перфорированный 100х100, 12 метров, и 10 гаек М10');
    const [isLoading, setIsLoading] = useState(false);
    const [error, setError] = useState('');
    const [successMessage, setSuccessMessage] = useState('');

    const handleGenerate = () => {
        if (!clientQuery.trim()) {
            setError('Пожалуйста, введите запрос клиента.');
            setSuccessMessage('');
            return;
        }

        setIsLoading(true);
        setError('');
        setSuccessMessage('');

        GenerateAndCreateFiles(clientQuery)
            .then(base64Data => {
                const link = document.createElement('a');
                link.href = `data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64,${base64Data}`;
                link.download = `ТКП_${Date.now()}.docx`;
                document.body.appendChild(link);
                link.click();
                document.body.removeChild(link);
                setSuccessMessage('ТКП успешно сгенерировано! Файл .docx скачивается, лог-файл .json сохранен рядом с приложением.');
            })
            .catch(err => {
                setError(`Ошибка: ${err}`);
            })
            .finally(() => {
                setIsLoading(false);
            });
    };

    return (
        <div className="app-container">
            <div className="card">
                <div className="header">
                    <h1>Авто-ТКП</h1>
                </div>
                <p className="subtitle">Интеллектуальный генератор коммерческих предложений</p>

                <div className="input-group">
                    <label htmlFor="clientQuery">Введите запрос клиента</label>
                    <textarea
                        id="clientQuery"
                        rows="5"
                        value={clientQuery}
                        onChange={(e) => setClientQuery(e.target.value)}
                        placeholder="Например: лоток 6000х200х100, 10 метров..."
                    />
                </div>

                {error && <div className="error-box">{error}</div>}
                {successMessage && <div className="success-box">{successMessage}</div>}

                <button onClick={handleGenerate} disabled={isLoading}>
                    {isLoading ? 'Генерация...' : 'Сгенерировать и скачать (.docx)'}
                </button>
            </div>
        </div>
    );
}

export default App;